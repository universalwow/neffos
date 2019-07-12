package neffos

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type (
	// Socket is the interface that an underline protocol implementation should implement.
	Socket interface {
		// NetConn returns the underline net connection.
		NetConn() net.Conn
		// Request returns the http request value.
		Request() *http.Request
		// ReadData reads binary or text messages from the remote connection.
		ReadData(timeout time.Duration) (body []byte, err error)
		// WriteBinary sends a binary message to the remote connection.
		WriteBinary(body []byte, timeout time.Duration) error
		// WriteText sends a text message to the remote connection.
		WriteText(body []byte, timeout time.Duration) error
	}
)

// Conn contains the websocket connection and the neffos communication functionality.
// Its `Connection` will return a new `NSConn` instance.
// Each connection can connect to one or more declared namespaces.
// Each `NSConn` can join to multiple rooms.
type Conn struct {
	// the ID generated by `Server#IDGenerator`.
	id string
	// serverConnID is unique per server instance and it can be comparable only within the
	// same server instance. Even if Server#IDGenerator
	// returns the same ID from the request.
	serverConnID string

	// the gorilla or gobwas socket.
	socket Socket
	// ReconnectTries, if > 0 then this connection is a result of a client-side reconnection,
	// see `WasReconnected() bool`.
	ReconnectTries int

	// non-nil if server-side connection.
	server *Server
	// when sever or client is ready to handle messages,
	// ack and queue is available,
	// see `Server#ServeHTTP.?OnConnect!=nil`.
	readiness *waiterOnce

	// maximum wait time allowed to read a message from the connection.
	// Defaults to no timeout.
	readTimeout time.Duration
	// maximum wait time allowed to write a message to the connection.
	// Defaults to no timeout.
	writeTimeout time.Duration

	// the defined namespaces, allowed to connect.
	namespaces Namespaces

	// more than 0 if acknowledged.
	acknowledged *uint32

	// the connection's current connected namespace.
	connectedNamespaces      map[string]*NSConn
	connectedNamespacesMutex sync.RWMutex
	// used to block certain actions until other action is finished,
	// i.e `askConnect: myNamespace` blocks the `tryNamespace: myNamespace` until finish.
	processes *processes

	// messages that this connection waits for a reply.
	waitingMessages      map[string]chan Message
	waitingMessagesMutex sync.RWMutex

	allowNativeMessages            bool
	shouldHandleOnlyNativeMessages bool

	queue      [][]byte
	queueMutex sync.Mutex

	// used to fire `conn#Close` once.
	closed *uint32
	// useful to terminate the broadcaster, see `Server#ServeHTTP.waitMessage`.
	closeCh chan struct{}
}

func newConn(socket Socket, namespaces Namespaces) *Conn {
	c := &Conn{
		socket:                         socket,
		namespaces:                     namespaces,
		readiness:                      newWaiterOnce(),
		acknowledged:                   new(uint32),
		connectedNamespaces:            make(map[string]*NSConn),
		processes:                      newProcesses(),
		waitingMessages:                make(map[string]chan Message),
		allowNativeMessages:            false,
		shouldHandleOnlyNativeMessages: false,
		closed:                         new(uint32),
		closeCh:                        make(chan struct{}),
	}

	if emptyNamespace := namespaces[""]; emptyNamespace != nil && emptyNamespace[OnNativeMessage] != nil {
		c.allowNativeMessages = true

		// if allow native messages and only this namespace empty namespaces is registered (via Events{} for example)
		// and the only one event is the `OnNativeMessage`
		// then no need to call Connect(...) because:
		// client-side can use raw websocket without the neffos.js library
		// so no access to connect to a namespace.
		if len(c.namespaces) == 1 && len(emptyNamespace) == 1 {
			c.connectedNamespaces[""] = newNSConn(c, "", emptyNamespace)
			c.shouldHandleOnlyNativeMessages = true
			atomic.StoreUint32(c.acknowledged, 1)
			c.readiness.unwait(nil)
		}
	}

	return c
}

// Is reports whether the "connID" is part of this server's connections and their IDs are equal.
func (c *Conn) Is(connID string) bool {
	if connID == "" {
		return false
	}

	if c.IsClient() {
		return c.id == connID
	}

	return c.serverConnID == connID
}

// ID method returns the unique identifier of the connection.
// If this is a server-side connection then this value is the generated one by the `Server#IDGenerator`.
// If this is a client-side connection then this value is filled on the acknowledgment process which is done on the `Client#Dial`.
func (c *Conn) ID() string {
	return c.id
}

// String method simply returns the ID(). Useful for fmt usage and
// to a connection to be passed on `Server#Broadcast` method
// to exclude itself from the broadcasted message's receivers.
func (c *Conn) String() string {
	return c.ID()
}

// Socket method returns the underline socket implementation.
func (c *Conn) Socket() Socket {
	return c.socket
}

// IsClient method reports whether this connections is a client-side connetion.
func (c *Conn) IsClient() bool {
	return c.server == nil
}

// Server method returns the backend server, it returns null on client-side connections.
func (c *Conn) Server() *Server {
	if c.IsClient() {
		return nil
	}

	return c.server
}

// WasReconnected reports whether the current connection is a result of a client-side reconnection.
// To get the numbers of total retries see the `ReconnectTries` field.
func (c *Conn) WasReconnected() bool {
	return c.ReconnectTries > 0
}

func (c *Conn) isAcknowledged() bool {
	return atomic.LoadUint32(c.acknowledged) > 0
}

const (
	ackBinary      = 'M' // byte(0x1) // comes from client to server at startup.
	ackIDBinary    = 'A' // byte(0x2) // comes from server to client after ackBinary and ready as a prefix, the rest message is the conn's ID.
	ackOKBinary    = 'K' // byte(0x3) // comes from client to server when id received and set-ed.
	ackNotOKBinary = 'H' // byte(0x4) // comes from server to client if `Server#OnConnected` errored as a prefix, the rest message is the error text.
)

func (c *Conn) sendClientACK() error {
	// if neffos client used but in reality nor of its features are used
	// because end-dev set it as native only sender and receiver so any webscoket client can be used
	// even the browser's default; we can't accept a custom ack neither a namespace connection or two-way error handling.
	if c.shouldHandleOnlyNativeMessages {
		return nil
	}

	ok := c.write([]byte{ackBinary}, false)
	if !ok {
		c.Close()
		return ErrWrite
	}

	err := c.readiness.wait()
	if err != nil {
		c.Close()
	}

	return err
}

func (c *Conn) startReader() {
	if c.IsClosed() {
		return
	}
	defer c.Close()

	// CLIENT is ready when ACK done
	// SERVER is ready when ACK is done AND `Server#OnConnected` returns with nil error.
	for {
		b, err := c.socket.ReadData(c.readTimeout)
		if err != nil {
			c.readiness.unwait(err)
			return
		}

		if len(b) == 0 {
			continue
		}

		if !c.isAcknowledged() {
			if !c.handleACK(b) {
				return
			}
			continue
		}

		c.HandlePayload(b)
	}
}

// ack uses binary, bytebuffer messages type, after this client/server can still use binary if `Message#SetBinary` or text message by-default.
func (c *Conn) handleACK(b []byte) bool {
	switch typ := b[0]; typ {
	case ackBinary:
		// from client startup to server.
		err := c.readiness.wait()
		if err != nil {
			// it's not Ok, send error which client's Dial should return.
			c.write(append([]byte{ackNotOKBinary}, []byte(err.Error())...), false)
			return false
		}
		atomic.StoreUint32(c.acknowledged, 1)
		c.handleQueue()

		// it's ok send ID.
		return c.write(append([]byte{ackIDBinary}, []byte(c.id)...), false)

	// case ackOKBinary:
	// 	// from client to server.

	// 	atomic.StoreUint32(c.acknowledged, 1)
	// 	c.handleQueue()

	case ackIDBinary:
		// from server to client.
		id := string(b[1:])
		c.id = id

		atomic.StoreUint32(c.acknowledged, 1)
		c.readiness.unwait(nil)
		// c.write([]byte{ackOKBinary})
		// println("ackIDBinary: pass with nil")
		// c.handleQueue()
	case ackNotOKBinary:
		// from server to client.
		errText := string(b[1:])
		err := errors.New(errText)
		c.readiness.unwait(err)
		return false
	default:
		c.queueMutex.Lock()
		c.queue = append(c.queue, b)
		c.queueMutex.Unlock()
	}

	return true

}

func (c *Conn) handleQueue() {
	c.queueMutex.Lock()
	defer c.queueMutex.Unlock()

	for _, b := range c.queue {
		c.HandlePayload(b)
	}

	c.queue = c.queue[0:0]
}

// ErrInvalidPayload can be returned by the internal `handleMessage`.
// In the future it may be exposed by an error listener.
var ErrInvalidPayload = errors.New("invalid payload")

func (c *Conn) handleMessage(msg Message) error {
	if msg.isInvalid {
		return ErrInvalidPayload
	}

	if msg.IsNative && c.allowNativeMessages {
		ns := c.Namespace("")
		return ns.events.fireEvent(ns, msg)
	}

	isClient := c.IsClient()
	if !isClient {
		c.server.waitingMessagesMutex.RLock()
		ch, ok := c.server.waitingMessages[msg.wait]
		c.server.waitingMessagesMutex.RUnlock()
		if ok {
			ch <- msg
			return nil
		}
	}

	if msg.isWait(isClient) {
		c.waitingMessagesMutex.RLock()
		ch, ok := c.waitingMessages[msg.wait]
		c.waitingMessagesMutex.RUnlock()
		if ok {
			ch <- msg
			return nil
		}
	}

	switch msg.Event {
	case OnNamespaceConnect:
		c.replyConnect(msg)
	case OnNamespaceDisconnect:
		c.replyDisconnect(msg)
	case OnRoomJoin:
		if ns, ok := c.tryNamespace(msg); ok {
			ns.replyRoomJoin(msg)
		}
	case OnRoomLeave:
		if ns, ok := c.tryNamespace(msg); ok {
			ns.replyRoomLeave(msg)
		}
	default:
		ns, ok := c.tryNamespace(msg)
		if !ok {
			// println(msg.Namespace + " namespace and incoming message of event: " + msg.Event + " is not connected or not exists and wait?: " + msg.wait + "\n\n")
			return ErrBadNamespace
		}

		msg.IsLocal = false
		err := ns.events.fireEvent(ns, msg)
		if err != nil {
			msg.Err = err
			c.Write(msg)
			return err
		}
	}

	return nil
}

// DeserializeMessage returns a Message from the "payload".
func (c *Conn) DeserializeMessage(payload []byte) Message {
	return deserializeMessage(nil, payload, c.allowNativeMessages, c.shouldHandleOnlyNativeMessages)
}

// HandlePayload fires manually a local event based on the "payload".
func (c *Conn) HandlePayload(payload []byte) error {
	return c.handleMessage(c.DeserializeMessage(payload))
}

const syncWaitDur = 15 * time.Millisecond

// 10 seconds is high value which is not realistic on healthy networks, but may useful for slow connections.
// This value is used just for the ack(which is usually done before the Connect call itself) wait on Connect when on server-side only.
const maxSyncWaitDur = 10 * time.Second

// Connect method returns a new connected to the specific "namespace" `NSConn` value.
// The "namespace" should be declared in the `connHandler` of both server and client sides.
// If this is a client-side connection then the server-side namespace's `OnNamespaceConnect` event callback MUST return null
// in order to allow this client-side connection to connect, otherwise a non-nil error is returned instead.
func (c *Conn) Connect(ctx context.Context, namespace string) (*NSConn, error) {
	// if c.IsClosed() {
	// 	return nil, ErrWrite
	// }

	if !c.IsClient() {
		c.readiness.unwait(nil)
		// server-side check for ack-ed, it should be done almost immediately the client connected
		// but give it sometime for slow networks and add an extra check for closed after 5 seconds and a deadline of 10seconds.
		t := maxSyncWaitDur
		for !c.isAcknowledged() {
			time.Sleep(syncWaitDur)
			t = -syncWaitDur

			if t <= maxSyncWaitDur/2 { // check once after 5 seconds if closed.
				if c.IsClosed() {
					return nil, ErrWrite
				}
			}

			if t == 0 {
				// when maxSyncWaitDur passed,
				// we could use the context's deadline but it will make things slower (extracting its value slower than the sleep time).
				if c.IsClosed() {
					return nil, ErrWrite
				}
				return nil, context.DeadlineExceeded
			}
		}
	}

	return c.askConnect(ctx, namespace)
}

// const defaultNS = ""

// func (c *Conn) DefaultNamespace() *NSConn {
// 	ns, _ := c.Connect(nil, defaultNS)
// 	return ns
// }

// WaitConnect method can be used instead of the `Connect` if the other side force-calls `Connect` to this connection
// and this side wants to "waits" for that signal.
//
// Nil context means try without timeout, wait until it connects to the specific namespace.
// Note that, this function will not return an `ErrBadNamespace` if namespace does not exist in the server-side
// or it's not defined in the client-side, it waits until deadline (if any, or loop forever, so a context with deadline is highly recommended).
func (c *Conn) WaitConnect(ctx context.Context, namespace string) (ns *NSConn, err error) {
	if ctx == nil {
		ctx = context.TODO()
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			if ns == nil {
				ns = c.Namespace(namespace)
			}

			if ns != nil && c.isAcknowledged() {
				return
			}

			time.Sleep(syncWaitDur)
		}
	}
}

// Namespace method returns an already-connected `NSConn` value based on the given "namespace".
func (c *Conn) Namespace(namespace string) *NSConn {
	c.connectedNamespacesMutex.RLock()
	ns := c.connectedNamespaces[namespace]
	c.connectedNamespacesMutex.RUnlock()

	return ns
}

func (c *Conn) tryNamespace(in Message) (*NSConn, bool) {
	// for atomic.LoadUint32(c.isConnectingProcess) > 0 {
	// }
	c.processes.get(in.Namespace).wait() // wait any `askConnect` process (if any) of that "in.Namespace".
	ns := c.Namespace(in.Namespace)
	if ns == nil {
		// if _, canConnect := c.namespaces[msg.Namespace]; !canConnect {
		// 	msg.Err = ErrForbiddenNamespace
		// }
		in.Err = ErrBadNamespace
		c.Write(in)
		return nil, false
	}

	return ns, true
}

// server#OnConnected -> conn#Connect
// client#WaitConnect
// or
// client#Connect
func (c *Conn) askConnect(ctx context.Context, namespace string) (*NSConn, error) {
	p := c.processes.get(namespace)
	p.start()      // block any `tryNamespace` with that "namespace".
	defer p.stop() // unblock.

	//	defer c.processes.get(namespace).run()()
	// for !atomic.CompareAndSwapUint32(c.isConnectingProcess, 0, 1) {
	// }
	// defer atomic.StoreUint32(c.isConnectingProcess, 0)
	ns := c.Namespace(namespace)
	if ns != nil {
		return ns, nil
	}

	events, ok := c.namespaces[namespace]
	if !ok {
		return nil, ErrBadNamespace
	}

	connectMessage := Message{
		Namespace: namespace,
		Event:     OnNamespaceConnect,
		IsLocal:   true,
	}

	ns = newNSConn(c, namespace, events)
	err := events.fireEvent(ns, connectMessage)
	if err != nil {
		return nil, err
	}

	// println("ask connect")
	_, err = c.Ask(ctx, connectMessage) // waits for answer no matter if already connected on the other side.
	if err != nil {
		return nil, err
	}
	// println("got connect")
	// re-check, maybe connected so far (can happen by a simultaneously `Connect` calls on both server and client, which is not the standard way)
	// c.connectedNamespacesMutex.RLock()
	// ns, ok = c.connectedNamespaces[namespace]
	// c.connectedNamespacesMutex.RUnlock()
	// if ok {
	// 	return ns, nil
	// }

	c.connectedNamespacesMutex.Lock()
	c.connectedNamespaces[namespace] = ns
	c.connectedNamespacesMutex.Unlock()

	// println("we're connected")

	// c.writeEmptyReply(genWaitConfirmation(reply.wait))
	// println("wrote: " + genWaitConfirmation(reply.wait))

	// c.sendConfirmation(reply.wait)

	c.notifyNamespaceConnected(ns, connectMessage)
	return ns, nil
}

func (c *Conn) replyConnect(msg Message) {
	// must give answer even a noOp if already connected.
	if msg.wait == "" || msg.isNoOp {
		return
	}

	ns := c.Namespace(msg.Namespace)
	if ns != nil {
		c.writeEmptyReply(msg.wait)
		return
	}

	events, ok := c.namespaces[msg.Namespace]
	if !ok {
		msg.Err = ErrBadNamespace
		c.Write(msg)
		return
	}

	ns = newNSConn(c, msg.Namespace, events)
	err := events.fireEvent(ns, msg)
	if err != nil {
		msg.Err = err
		c.Write(msg)
		return
	}

	c.connectedNamespacesMutex.Lock()
	c.connectedNamespaces[msg.Namespace] = ns
	c.connectedNamespacesMutex.Unlock()

	c.writeEmptyReply(msg.wait)

	c.notifyNamespaceConnected(ns, msg)
}

func (c *Conn) notifyNamespaceConnected(ns *NSConn, connectMsg Message) {
	connectMsg.Event = OnNamespaceConnected
	ns.events.fireEvent(ns, connectMsg) // omit error, it's connected.

	if !c.IsClient() && c.server.usesStackExchange() {
		c.server.StackExchange.Subscribe(c, ns.namespace)
	}
}

func (c *Conn) notifyNamespaceDisconnect(ns *NSConn, disconnectMsg Message) {
	if !c.IsClient() && c.server.usesStackExchange() {
		c.server.StackExchange.Unsubscribe(c, disconnectMsg.Namespace)
	}
}

// DisconnectAll method disconnects from all namespaces,
// `OnNamespaceDisconnect` even will be fired and its `Message.IsLocal` will be true.
// The remote side gets notified.
func (c *Conn) DisconnectAll(ctx context.Context) error {
	if c.shouldHandleOnlyNativeMessages {
		return nil
	}

	c.connectedNamespacesMutex.Lock()
	defer c.connectedNamespacesMutex.Unlock()

	disconnectMsg := Message{Event: OnNamespaceDisconnect, IsLocal: true, locked: true}
	for namespace := range c.connectedNamespaces {
		disconnectMsg.Namespace = namespace
		if err := c.askDisconnect(ctx, disconnectMsg, false); err != nil {
			return err
		}
	}

	return nil
}

func (c *Conn) askDisconnect(ctx context.Context, msg Message, lock bool) error {
	if lock {
		c.connectedNamespacesMutex.RLock()
	}

	ns := c.connectedNamespaces[msg.Namespace]

	if lock {
		c.connectedNamespacesMutex.RUnlock()
	}

	if ns == nil {
		return ErrBadNamespace
	}

	_, err := c.Ask(ctx, msg)
	if err != nil {
		return err
	}

	// if disconnect is allowed then leave rooms first with force property
	// before namespace's deletion.
	ns.forceLeaveAll(true)

	if lock {
		c.connectedNamespacesMutex.Lock()
	}

	delete(c.connectedNamespaces, msg.Namespace)

	if lock {
		c.connectedNamespacesMutex.Unlock()
	}

	msg.IsLocal = true
	ns.events.fireEvent(ns, msg)

	c.notifyNamespaceDisconnect(ns, msg)
	return nil
}

func (c *Conn) replyDisconnect(msg Message) {
	if msg.wait == "" || msg.isNoOp {
		return
	}

	ns := c.Namespace(msg.Namespace)
	if ns == nil {
		c.writeEmptyReply(msg.wait)
		return
	}

	// if client then we need to respond to server and delete the namespace without ask the local event.
	if c.IsClient() {
		// if disconnect is allowed then leave rooms first with force property
		// before namespace's deletion.
		ns.forceLeaveAll(false)

		c.connectedNamespacesMutex.Lock()
		delete(c.connectedNamespaces, msg.Namespace)
		c.connectedNamespacesMutex.Unlock()

		c.writeEmptyReply(msg.wait)

		ns.events.fireEvent(ns, msg)
		return
	}

	// server-side, check for error on the local event first.
	err := ns.events.fireEvent(ns, msg)
	if err != nil {
		msg.Err = err
		c.Write(msg)
		return
	}

	ns.forceLeaveAll(false)

	c.connectedNamespacesMutex.Lock()
	delete(c.connectedNamespaces, msg.Namespace)
	c.connectedNamespacesMutex.Unlock()

	c.notifyNamespaceDisconnect(ns, msg)

	c.writeEmptyReply(msg.wait)
}

func (c *Conn) write(b []byte, binary bool) bool {
	var err error
	if binary {
		err = c.socket.WriteBinary(b, c.writeTimeout)
	} else {
		err = c.socket.WriteText(b, c.writeTimeout)
	}

	if err != nil {
		if IsCloseError(err) {
			c.Close()
		}
		return false
	}

	return true
}

func (c *Conn) canWrite(msg Message) bool {
	if c.IsClosed() {
		return false
	}

	if !c.IsClient() {
		// for server-side if tries to send, then error will be not ignored but events should continue.
		c.readiness.unwait(nil)
	}

	if !msg.isConnect() && !msg.isDisconnect() {
		if !msg.locked {
			c.connectedNamespacesMutex.RLock()
		}

		ns := c.connectedNamespaces[msg.Namespace]

		if !msg.locked {
			c.connectedNamespacesMutex.RUnlock()
		}

		if ns == nil {
			return false
		}

		if msg.Room != "" && !msg.isRoomJoin() && !msg.isRoomLeft() {
			if !msg.locked {
				ns.roomsMutex.RLock()
			}

			_, ok := ns.rooms[msg.Room]

			if !msg.locked {
				ns.roomsMutex.RUnlock()
			}

			if !ok {
				// tried to send to a not joined room.
				return false
			}
		}
	}

	// if !c.IsClient() && !msg.FromStackExchange {
	// 	if exc := c.Server().StackExchange; exc != nil {
	// 		if exc.Publish(c, msg) {
	// 			return true
	// 		}
	// 	}
	// }

	// don't write if explicit "from" field is set
	// to this server's instance client connection ~~~but give a chance to Publish
	// it to other instances with the same conn ID, if any~~~.
	if c.Is(msg.FromExplicit) {
		return false
	}

	return true
}

// Write method sends a message to the remote side,
// reports whether the connection is still available
// or when this message is not allowed to be sent to the remote side.
func (c *Conn) Write(msg Message) bool {
	if !c.canWrite(msg) {
		return false
	}

	msg.FromExplicit = ""
	b := serializeMessage(nil, msg)
	return c.write(b, msg.SetBinary)
}

// used when `Ask` caller cares only for successful call and not the message, for performance reasons we just use raw bytes.
func (c *Conn) writeEmptyReply(wait string) bool {
	return c.write(genEmptyReplyToWait(wait), false)
}

func (c *Conn) waitConfirmation(wait string) {
	wait = genWaitConfirmation(wait)

	ch := make(chan Message)
	c.waitingMessagesMutex.Lock()
	c.waitingMessages[wait] = ch
	c.waitingMessagesMutex.Unlock()
	<-ch
}

func (c *Conn) sendConfirmation(wait string) {
	wait = genWaitConfirmation(wait)
	c.writeEmptyReply(wait)
}

// Ask method sends a message to the remote side and blocks until a response or an error received from the specific `Message.Event`.
func (c *Conn) Ask(ctx context.Context, msg Message) (Message, error) {
	if c.shouldHandleOnlyNativeMessages {
		// should panic or...
		return Message{}, nil
	}

	if c.IsClosed() {
		return msg, CloseError{Code: -1, error: ErrWrite}
	}

	msg.wait = genWait(c.IsClient())

	if ctx == nil {
		ctx = context.TODO()
	} else {
		if deadline, has := ctx.Deadline(); has {
			if deadline.Before(time.Now().Add(-1 * time.Second)) {
				return Message{}, context.DeadlineExceeded
			}
		}
	}

	ch := make(chan Message)
	c.waitingMessagesMutex.Lock()
	c.waitingMessages[msg.wait] = ch
	c.waitingMessagesMutex.Unlock()

	if !c.Write(msg) {
		// println("fail to write connect message.")
		return Message{}, ErrWrite
	}

	select {
	case <-ctx.Done():
		if c.IsClosed() {
			return Message{}, ErrWrite
		}
		return Message{}, ctx.Err()
	case receive := <-ch:
		c.waitingMessagesMutex.Lock()
		delete(c.waitingMessages, msg.wait)
		c.waitingMessagesMutex.Unlock()

		return receive, receive.Err
	}
}

// Close method will force-disconnect from all connected namespaces and force-leave from all joined rooms
// and finally will terminate the underline websocket connection.
// After this method call the `Conn` is not usable anymore, a new `Dial` call is required.
func (c *Conn) Close() {
	if atomic.CompareAndSwapUint32(c.closed, 0, 1) {
		if !c.shouldHandleOnlyNativeMessages {
			disconnectMsg := Message{Event: OnNamespaceDisconnect, IsForced: true, IsLocal: true}
			c.connectedNamespacesMutex.Lock()
			for namespace, ns := range c.connectedNamespaces {
				// leave rooms first with force and local property before remove the namespace completely.
				ns.forceLeaveAll(true)

				disconnectMsg.Namespace = ns.namespace
				ns.events.fireEvent(ns, disconnectMsg)
				delete(c.connectedNamespaces, namespace)
			}
			c.connectedNamespacesMutex.Unlock()

			c.waitingMessagesMutex.Lock()
			for wait := range c.waitingMessages {
				delete(c.waitingMessages, wait)
			}
			c.waitingMessagesMutex.Unlock()
		}

		atomic.StoreUint32(c.acknowledged, 0)

		go func() {
			if !c.IsClient() {
				c.server.disconnect <- c
			}
		}()

		close(c.closeCh)
		c.socket.NetConn().Close()
	}
}

// IsClosed method reports whether this connection is remotely or manually terminated.
func (c *Conn) IsClosed() bool {
	return atomic.LoadUint32(c.closed) > 0
}
