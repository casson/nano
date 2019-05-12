// Copyright (c) nano Author. All Rights Reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package cluster

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
	"github.com/lonng/nano/cluster/clusterpb"
	"github.com/lonng/nano/component"
	"github.com/lonng/nano/internal/env"
	"github.com/lonng/nano/internal/log"
	"github.com/lonng/nano/pipeline"
	"google.golang.org/grpc"
)

// Node represents a node in nano cluster, which will contains a group of services.
// All services will register to cluster and messages will be forwarded to the node
// which provides respective service
type Node struct {
	IsMaster       bool
	AdvertiseAddr  string
	MemberAddr     string
	ServerAddr     string
	Components     *component.Components
	IsWebsocket    bool
	TSLCertificate string
	TSLKey         string
	Pipeline       pipeline.Pipeline

	cluster      *cluster
	handler      *LocalHandler
	masterServer *grpc.Server
	memberServer *grpc.Server
	rpcClient    *rpcClient
}

func (n *Node) Startup() error {
	if n.IsMaster && n.AdvertiseAddr == "" {
		return errors.New("advertise address cannot be empty in master node")
	}
	n.cluster = newCluster(n)
	n.handler = NewHandler(n.Pipeline)
	components := n.Components.List()
	for _, c := range components {
		err := n.handler.register(c.Comp, c.Opts)
		if err != nil {
			return err
		}
	}
	cache()

	// Bootstrap cluster if either current is master or advertise address is not empty
	// - Current node is master
	// - Current node is cluster member
	if n.IsMaster {
		err := n.initMaster()
		if err != nil {
			return err
		}
	} else if n.AdvertiseAddr != "" {
		err := n.initMember()
		if err != nil {
			return err
		}
	}

	// Initialize all components
	for _, c := range components {
		c.Comp.Init()
	}
	for _, c := range components {
		c.Comp.AfterInit()
	}

	go func() {
		if n.IsWebsocket {
			if len(n.TSLCertificate) != 0 {
				n.listenAndServeWSTLS()
			} else {
				n.listenAndServeWS()
			}
		} else {
			n.listenAndServe()
		}
	}()
	return nil
}

func (n *Node) Handler() *LocalHandler {
	return n.handler
}

func (n *Node) initMaster() error {
	listener, err := net.Listen("tcp", n.AdvertiseAddr)
	if err != nil {
		return err
	}
	n.masterServer = grpc.NewServer()
	n.memberServer = grpc.NewServer()
	clusterpb.RegisterMasterServer(n.masterServer, n.cluster)
	clusterpb.RegisterMemberServer(n.memberServer, n)
	go func() {
		err := n.masterServer.Serve(listener)
		if err != nil {
			log.Println("Start master node failed: " + err.Error())
		}
	}()
	n.rpcClient = newRPCClient()
	member := &Member{
		isMaster: true,
		memberInfo: &clusterpb.MemberInfo{
			MemberType: "master",
			MemberAddr: n.MemberAddr,
			Services:   n.handler.LocalService(),
		},
	}
	n.cluster.members = append(n.cluster.members, member)
	n.cluster.setRpcClient(n.rpcClient)
	n.handler.setRpcClient(n.rpcClient)
	return nil
}

func (n *Node) initMember() error {
	if n.MemberAddr == "" || !strings.Contains(n.MemberAddr, ":") || strings.SplitN(n.MemberAddr, ":", 2)[0] == "" {
		return fmt.Errorf("member address (%s) invalid in cluster mode", n.MemberAddr)
	}

	listener, err := net.Listen("tcp", n.MemberAddr)
	if err != nil {
		return err
	}

	n.memberServer = grpc.NewServer()
	clusterpb.RegisterMemberServer(n.memberServer, n)
	go func() {
		err := n.memberServer.Serve(listener)
		if err != nil {
			log.Println("Start master node failed: " + err.Error())
		}
	}()

	// Register current node to master
	n.rpcClient = newRPCClient()
	conns, err := n.rpcClient.getConnArray(n.AdvertiseAddr)
	if err != nil {
		return err
	}
	client := clusterpb.NewMasterClient(conns.Get())
	request := &clusterpb.RegisterRequest{
		MemberInfo: &clusterpb.MemberInfo{
			MemberType: "member",
			MemberAddr: n.MemberAddr,
			Services:   n.handler.LocalService(),
		},
	}
	resp, err := client.Register(context.Background(), request)
	if err != nil {
		return err
	}
	n.handler.initRemoteService(resp.Members)
	n.handler.setRpcClient(n.rpcClient)
	return nil
}

// Shutdowns all components registered by application, that
// call by reverse order against register
func (n *Node) Shutdown() {
	// reverse call `BeforeShutdown` hooks
	components := n.Components.List()
	length := len(components)
	for i := length - 1; i >= 0; i-- {
		components[i].Comp.BeforeShutdown()
	}

	// reverse call `Shutdown` hooks
	for i := length - 1; i >= 0; i-- {
		components[i].Comp.Shutdown()
	}

	if n.masterServer != nil {
		n.masterServer.GracefulStop()
	}
}

// Enable current server accept connection
func (n *Node) listenAndServe() {
	listener, err := net.Listen("tcp", n.ServerAddr)
	if err != nil {
		log.Fatal(err.Error())
	}

	defer listener.Close()
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Println(err.Error())
			continue
		}

		go n.handler.handle(conn)
	}
}

func (n *Node) listenAndServeWS() {
	var upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     env.CheckOrigin,
	}

	http.HandleFunc("/"+strings.TrimPrefix(env.WSPath, "/"), func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Println(fmt.Sprintf("Upgrade failure, URI=%s, Error=%s", r.RequestURI, err.Error()))
			return
		}

		n.handler.handleWS(conn)
	})

	if err := http.ListenAndServe(n.ServerAddr, nil); err != nil {
		log.Fatal(err.Error())
	}
}

func (n *Node) listenAndServeWSTLS() {
	var upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     env.CheckOrigin,
	}

	http.HandleFunc("/"+strings.TrimPrefix(env.WSPath, "/"), func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Println(fmt.Sprintf("Upgrade failure, URI=%s, Error=%s", r.RequestURI, err.Error()))
			return
		}

		n.handler.handleWS(conn)
	})

	if err := http.ListenAndServeTLS(n.ServerAddr, n.TSLCertificate, n.TSLKey, nil); err != nil {
		log.Fatal(err.Error())
	}
}

func (n *Node) HandleRequest(context.Context, *clusterpb.RequestMessage) (*clusterpb.MemberHandleResponse, error) {
	panic("implement me")
}

func (n *Node) HandleNotify(context.Context, *clusterpb.NotifyMessage) (*clusterpb.MemberHandleResponse, error) {
	panic("implement me")
}

func (n *Node) HandlePush(context.Context, *clusterpb.PushMessage) (*clusterpb.MemberHandleResponse, error) {
	panic("implement me")
}

func (n *Node) HandleResponse(context.Context, *clusterpb.ResponseMessage) (*clusterpb.MemberHandleResponse, error) {
	panic("implement me")
}

func (n *Node) NewMember(_ context.Context, req *clusterpb.NewMemberRequest) (*clusterpb.NewMemberResponse, error) {
	n.handler.addRemoteService(req.MemberInfo)
	return &clusterpb.NewMemberResponse{}, nil
}
