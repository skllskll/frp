// Copyright 2017 fatedier, fatedier@gmail.com
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package visitor

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/fatedier/golib/errors"
	libio "github.com/fatedier/golib/io"

	"github.com/fatedier/frp/pkg/config"
	"github.com/fatedier/frp/pkg/msg"
	"github.com/fatedier/frp/pkg/proto/udp"
	utilnet "github.com/fatedier/frp/pkg/util/net"
	"github.com/fatedier/frp/pkg/util/util"
	"github.com/fatedier/frp/pkg/util/xlog"
)

type SUDPVisitor struct {
	*BaseVisitor

	checkCloseCh chan struct{}
	// udpConn is the listener of udp packet
	udpConn *net.UDPConn
	readCh  chan *msg.UDPPacket
	sendCh  chan *msg.UDPPacket

	cfg *config.SUDPVisitorConf
}

// SUDP Run start listen a udp port
func (sv *SUDPVisitor) Run() (err error) {
	xl := xlog.FromContextSafe(sv.ctx)

	addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(sv.cfg.BindAddr, strconv.Itoa(sv.cfg.BindPort)))
	if err != nil {
		return fmt.Errorf("sudp ResolveUDPAddr error: %v", err)
	}

	sv.udpConn, err = net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("listen udp port %s error: %v", addr.String(), err)
	}

	sv.sendCh = make(chan *msg.UDPPacket, 1024)
	sv.readCh = make(chan *msg.UDPPacket, 1024)

	xl.Info("sudp start to work, listen on %s", addr)

	go sv.dispatcher()
	go udp.ForwardUserConn(sv.udpConn, sv.readCh, sv.sendCh, int(sv.clientCfg.UDPPacketSize))

	return
}

func (sv *SUDPVisitor) dispatcher() {
	xl := xlog.FromContextSafe(sv.ctx)

	var (
		visitorConn net.Conn
		err         error

		firstPacket *msg.UDPPacket
	)

	for {
		select {
		case firstPacket = <-sv.sendCh:
			if firstPacket == nil {
				xl.Info("frpc sudp visitor proxy is closed")
				return
			}
		case <-sv.checkCloseCh:
			xl.Info("frpc sudp visitor proxy is closed")
			return
		}

		visitorConn, err = sv.getNewVisitorConn()
		if err != nil {
			xl.Warn("newVisitorConn to frps error: %v, try to reconnect", err)
			continue
		}

		// visitorConn always be closed when worker done.
		sv.worker(visitorConn, firstPacket)

		select {
		case <-sv.checkCloseCh:
			return
		default:
		}
	}
}

func (sv *SUDPVisitor) worker(workConn net.Conn, firstPacket *msg.UDPPacket) {
	xl := xlog.FromContextSafe(sv.ctx)
	xl.Debug("starting sudp proxy worker")

	wg := &sync.WaitGroup{}
	wg.Add(2)
	closeCh := make(chan struct{})

	// udp service -> frpc -> frps -> frpc visitor -> user
	workConnReaderFn := func(conn net.Conn) {
		defer func() {
			conn.Close()
			close(closeCh)
			wg.Done()
		}()

		for {
			var (
				rawMsg msg.Message
				errRet error
			)

			// frpc will send heartbeat in workConn to frpc visitor for keeping alive
			_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
			if rawMsg, errRet = msg.ReadMsg(conn); errRet != nil {
				xl.Warn("read from workconn for user udp conn error: %v", errRet)
				return
			}

			_ = conn.SetReadDeadline(time.Time{})
			switch m := rawMsg.(type) {
			case *msg.Ping:
				xl.Debug("frpc visitor get ping message from frpc")
				continue
			case *msg.UDPPacket:
				if errRet := errors.PanicToError(func() {
					sv.readCh <- m
					xl.Trace("frpc visitor get udp packet from workConn: %s", m.Content)
				}); errRet != nil {
					xl.Info("reader goroutine for udp work connection closed")
					return
				}
			}
		}
	}

	// udp service <- frpc <- frps <- frpc visitor <- user
	workConnSenderFn := func(conn net.Conn) {
		defer func() {
			conn.Close()
			wg.Done()
		}()

		var errRet error
		if firstPacket != nil {
			if errRet = msg.WriteMsg(conn, firstPacket); errRet != nil {
				xl.Warn("sender goroutine for udp work connection closed: %v", errRet)
				return
			}
			xl.Trace("send udp package to workConn: %s", firstPacket.Content)
		}

		for {
			select {
			case udpMsg, ok := <-sv.sendCh:
				if !ok {
					xl.Info("sender goroutine for udp work connection closed")
					return
				}

				if errRet = msg.WriteMsg(conn, udpMsg); errRet != nil {
					xl.Warn("sender goroutine for udp work connection closed: %v", errRet)
					return
				}
				xl.Trace("send udp package to workConn: %s", udpMsg.Content)
			case <-closeCh:
				return
			}
		}
	}

	go workConnReaderFn(workConn)
	go workConnSenderFn(workConn)

	wg.Wait()
	xl.Info("sudp worker is closed")
}

func (sv *SUDPVisitor) getNewVisitorConn() (net.Conn, error) {
	xl := xlog.FromContextSafe(sv.ctx)
	visitorConn, err := sv.connectServer()
	if err != nil {
		return nil, fmt.Errorf("frpc connect frps error: %v", err)
	}

	now := time.Now().Unix()
	newVisitorConnMsg := &msg.NewVisitorConn{
		ProxyName:      sv.cfg.ServerName,
		SignKey:        util.GetAuthKey(sv.cfg.Sk, now),
		Timestamp:      now,
		UseEncryption:  sv.cfg.UseEncryption,
		UseCompression: sv.cfg.UseCompression,
	}
	err = msg.WriteMsg(visitorConn, newVisitorConnMsg)
	if err != nil {
		return nil, fmt.Errorf("frpc send newVisitorConnMsg to frps error: %v", err)
	}

	var newVisitorConnRespMsg msg.NewVisitorConnResp
	_ = visitorConn.SetReadDeadline(time.Now().Add(10 * time.Second))
	err = msg.ReadMsgInto(visitorConn, &newVisitorConnRespMsg)
	if err != nil {
		return nil, fmt.Errorf("frpc read newVisitorConnRespMsg error: %v", err)
	}
	_ = visitorConn.SetReadDeadline(time.Time{})

	if newVisitorConnRespMsg.Error != "" {
		return nil, fmt.Errorf("start new visitor connection error: %s", newVisitorConnRespMsg.Error)
	}

	var remote io.ReadWriteCloser
	remote = visitorConn
	if sv.cfg.UseEncryption {
		remote, err = libio.WithEncryption(remote, []byte(sv.cfg.Sk))
		if err != nil {
			xl.Error("create encryption stream error: %v", err)
			return nil, err
		}
	}
	if sv.cfg.UseCompression {
		remote = libio.WithCompression(remote)
	}
	return utilnet.WrapReadWriteCloserToConn(remote, visitorConn), nil
}

func (sv *SUDPVisitor) Close() {
	sv.mu.Lock()
	defer sv.mu.Unlock()

	select {
	case <-sv.checkCloseCh:
		return
	default:
		close(sv.checkCloseCh)
	}
	sv.BaseVisitor.Close()
	if sv.udpConn != nil {
		sv.udpConn.Close()
	}
	close(sv.readCh)
	close(sv.sendCh)
}
