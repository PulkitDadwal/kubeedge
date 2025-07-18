package conn

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"k8s.io/klog/v2"

	"github.com/kubeedge/beehive/pkg/core/model"
	"github.com/kubeedge/kubeedge/pkg/viaduct/pkg/api"
	"github.com/kubeedge/kubeedge/pkg/viaduct/pkg/comm"
	"github.com/kubeedge/kubeedge/pkg/viaduct/pkg/fifo"
	"github.com/kubeedge/kubeedge/pkg/viaduct/pkg/keeper"
	"github.com/kubeedge/kubeedge/pkg/viaduct/pkg/lane"
	"github.com/kubeedge/kubeedge/pkg/viaduct/pkg/mux"
)

type WSConnection struct {
	WriteDeadline      time.Time
	ReadDeadline       time.Time
	handler            mux.Handler
	wsConn             *websocket.Conn
	state              *ConnectionState
	syncKeeper         *keeper.SyncKeeper
	connUse            api.UseType
	consumer           io.Writer
	autoRoute          bool
	messageFifo        *fifo.MessageFifo
	locker             sync.Mutex
	OnReadTransportErr func(nodeID, projectID string)
}

func NewWSConn(options *ConnectionOptions) *WSConnection {
	return &WSConnection{
		wsConn:             options.Base.(*websocket.Conn),
		handler:            options.Handler,
		syncKeeper:         keeper.NewSyncKeeper(),
		state:              options.State,
		connUse:            options.ConnUse,
		autoRoute:          options.AutoRoute,
		messageFifo:        fifo.NewMessageFifo(),
		OnReadTransportErr: options.OnReadTransportErr,
	}
}

// ServeConn start to receive message from connection
func (conn *WSConnection) ServeConn() {
	switch conn.connUse {
	case api.UseTypeMessage:
		go conn.handleMessage()
	case api.UseTypeStream:
		go conn.handleRawData()
	case api.UseTypeShare:
		klog.Error("don't support share in websocket")
	}
}

func (conn *WSConnection) filterControlMessage(msg *model.Message) bool {
	// check control message
	operation := msg.GetOperation()
	if operation != comm.ControlTypeConfig &&
		operation != comm.ControlTypePing &&
		operation != comm.ControlTypePong {
		return false
	}

	// feedback the response
	resp := msg.NewRespByMessage(msg, comm.RespTypeAck)
	conn.locker.Lock()
	err := lane.NewLane(api.ProtocolTypeWS, conn.wsConn).WriteMessage(resp)
	conn.locker.Unlock()
	if err != nil {
		klog.Errorf("failed to send response back, error:%+v", err)
	}
	return true
}

func (conn *WSConnection) handleRawData() {
	if conn.consumer == nil {
		klog.Warning("bad consumer for raw data")
		return
	}

	if !conn.autoRoute {
		return
	}

	// TODO: support control message processing in raw data mode
	_, err := io.Copy(conn.consumer, lane.NewLane(api.ProtocolTypeQuic, conn.wsConn))
	if err != nil {
		klog.Errorf("failed to copy data, error: %+v", err)
		conn.state.State = api.StatDisconnected
		conn.wsConn.Close()
		return
	}
}

func (conn *WSConnection) handleMessage() {
	for {
		msg := &model.Message{}
		err := lane.NewLane(api.ProtocolTypeWS, conn.wsConn).ReadMessage(msg)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				klog.Errorf("failed to read message, error: %+v", err)
			}
			conn.state.State = api.StatDisconnected
			_ = conn.wsConn.Close()

			if conn.OnReadTransportErr != nil {
				conn.OnReadTransportErr(conn.state.Headers.Get("node_id"),
					conn.state.Headers.Get("project_id"))
			}

			return
		}

		// filter control message
		if filtered := conn.filterControlMessage(msg); filtered {
			continue
		}

		// to check whether the message is a response or not
		if matched := conn.syncKeeper.MatchAndNotify(*msg); matched {
			continue
		}

		// put the messages into fifo and wait for reading
		if !conn.autoRoute {
			conn.messageFifo.Put(msg)
			continue
		}

		if conn.handler == nil {
			// use default mux
			conn.handler = mux.MuxDefault
		}
		conn.handler.ServeConn(&mux.MessageRequest{
			Header:           conn.state.Headers,
			PeerCertificates: conn.state.PeerCertificates,
			Message:          msg,
		}, &responseWriter{
			Type: api.ProtocolTypeWS,
			Van:  conn.wsConn,
		})
	}
}

func (conn *WSConnection) SetReadDeadline(t time.Time) error {
	conn.ReadDeadline = t
	conn.locker.Lock()
	defer conn.locker.Unlock()
	if conn.wsConn != nil {
		return conn.wsConn.SetReadDeadline(t)
	}
	return nil
}

func (conn *WSConnection) SetWriteDeadline(t time.Time) error {
	conn.WriteDeadline = t
	return nil
}

func (conn *WSConnection) Read(raw []byte) (int, error) {
	return lane.NewLane(api.ProtocolTypeWS, conn.wsConn).Read(raw)
}

func (conn *WSConnection) Write(raw []byte) (int, error) {
	return lane.NewLane(api.ProtocolTypeWS, conn.wsConn).Write(raw)
}

func (conn *WSConnection) WriteMessageAsync(msg *model.Message) error {
	lane := lane.NewLane(api.ProtocolTypeWS, conn.wsConn)
	_ = lane.SetWriteDeadline(conn.WriteDeadline)
	msg.Header.Sync = false
	conn.locker.Lock()
	defer conn.locker.Unlock()
	return lane.WriteMessage(msg)
}

func (conn *WSConnection) WriteMessageSync(msg *model.Message) (*model.Message, error) {
	lane := lane.NewLane(api.ProtocolTypeWS, conn.wsConn)
	// send msg
	_ = lane.SetWriteDeadline(conn.WriteDeadline)
	msg.Header.Sync = true
	conn.locker.Lock()
	err := lane.WriteMessage(msg)
	conn.locker.Unlock()
	if err != nil {
		klog.Errorf("write message error(%+v)", err)
		return nil, err
	}
	//receive response
	response, err := conn.syncKeeper.WaitResponse(msg, conn.WriteDeadline)
	return &response, err
}

func (conn *WSConnection) ReadMessage(msg *model.Message) error {
	return conn.messageFifo.Get(msg)
}

func (conn *WSConnection) RemoteAddr() net.Addr {
	conn.locker.Lock()
	defer conn.locker.Unlock()
	if conn.wsConn != nil {
		return conn.wsConn.RemoteAddr()
	}
	return nil
}

func (conn *WSConnection) LocalAddr() net.Addr {
	conn.locker.Lock()
	defer conn.locker.Unlock()
	if conn.wsConn != nil {
		return conn.wsConn.LocalAddr()
	}
	return nil
}

func (conn *WSConnection) Close() error {
	conn.messageFifo.Close()
	conn.locker.Lock()
	defer conn.locker.Unlock()
	if conn.wsConn != nil {
		return conn.wsConn.Close()
	}
	return nil
}

// get connection state
// TODO:
func (conn *WSConnection) ConnectionState() ConnectionState {
	return *conn.state
}
