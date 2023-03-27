package imapserver

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

const (
	cmdReadTimeout     = 30 * time.Second
	idleReadTimeout    = 35 * time.Minute // section 5.4 says 30min minimum
	literalReadTimeout = 5 * time.Minute

	respWriteTimeout    = 30 * time.Second
	literalWriteTimeout = 5 * time.Minute
)

var internalServerErrorResp = &imap.StatusResponse{
	Type: imap.StatusResponseTypeNo,
	Code: imap.ResponseCodeServerBug,
	Text: "Internal server error",
}

type conn struct {
	conn     net.Conn
	server   *Server
	br       *bufio.Reader
	bw       *bufio.Writer
	encMutex sync.Mutex

	mutex   sync.Mutex
	enabled imap.CapSet

	state   imap.ConnState
	session Session
}

func newConn(c net.Conn, server *Server) *conn {
	br := bufio.NewReader(c)
	bw := bufio.NewWriter(c)
	return &conn{
		conn:    c,
		server:  server,
		br:      br,
		bw:      bw,
		enabled: make(imap.CapSet),
	}
}

func (c *conn) serve() {
	defer func() {
		if v := recover(); v != nil {
			c.server.logger().Printf("panic handling command: %v\n%s", v, debug.Stack())
		}

		c.conn.Close()
	}()

	var err error
	c.session, err = c.server.NewSession()
	if err != nil {
		var (
			resp    *imap.StatusResponse
			imapErr *imap.Error
		)
		if errors.As(err, &imapErr) {
			resp = (*imap.StatusResponse)(imapErr)
		} else {
			c.server.logger().Printf("failed to create session: %v", err)
			resp = internalServerErrorResp
		}
		if err := c.writeStatusResp("", resp); err != nil {
			c.server.logger().Printf("failed to write greeting: %v", err)
		}
		return
	}

	defer func() {
		if c.session != nil {
			if err := c.session.Close(); err != nil {
				c.server.logger().Printf("failed to close session: %v", err)
			}
		}
	}()

	c.state = imap.ConnStateNotAuthenticated
	if err := c.writeGreeting(); err != nil {
		c.server.logger().Printf("failed to write greeting: %v", err)
		return
	}

	for {
		var readTimeout time.Duration
		switch c.state {
		case imap.ConnStateAuthenticated, imap.ConnStateSelected:
			readTimeout = idleReadTimeout
		default:
			readTimeout = cmdReadTimeout
		}
		c.setReadTimeout(readTimeout)

		dec := imapwire.NewDecoder(c.br, imapwire.ConnSideServer)
		dec.CheckBufferedLiteralFunc = c.checkBufferedLiteral

		if c.state == imap.ConnStateLogout || dec.EOF() {
			break
		}

		c.setReadTimeout(cmdReadTimeout)
		if err := c.readCommand(dec); err != nil {
			c.server.logger().Printf("failed to read command: %v", err)
			break
		}
	}
}

func (c *conn) readCommand(dec *imapwire.Decoder) error {
	var tag, name string
	if !dec.ExpectAtom(&tag) || !dec.ExpectSP() || !dec.ExpectAtom(&name) {
		return fmt.Errorf("in command: %w", dec.Err())
	}
	name = strings.ToUpper(name)

	numKind := NumKindSeq
	if name == "UID" {
		numKind = NumKindUID
		var subName string
		if !dec.ExpectSP() || !dec.ExpectAtom(&subName) {
			return fmt.Errorf("in command: %w", dec.Err())
		}
		name = "UID " + strings.ToUpper(subName)
	}

	// TODO: handle multiple commands concurrently
	sendOK := true
	var err error
	switch name {
	case "NOOP":
		err = c.handleNoop(dec)
	case "LOGOUT":
		err = c.handleLogout(dec)
	case "CAPABILITY":
		err = c.handleCapability(dec)
	case "STARTTLS":
		err = c.handleStartTLS(tag, dec)
		sendOK = false
	case "AUTHENTICATE":
		err = c.handleAuthenticate(dec)
	case "LOGIN":
		err = c.handleLogin(dec)
	case "ENABLE":
		err = c.handleEnable(dec)
	case "CREATE":
		err = c.handleCreate(dec)
	case "DELETE":
		err = c.handleDelete(dec)
	case "RENAME":
		err = c.handleRename(dec)
	case "SUBSCRIBE":
		err = c.handleSubscribe(dec)
	case "UNSUBSCRIBE":
		err = c.handleUnsubscribe(dec)
	case "STATUS":
		err = c.handleStatus(dec)
	case "LIST":
		err = c.handleList(dec)
	case "NAMESPACE":
		err = c.handleNamespace(dec)
	case "IDLE":
		err = c.handleIdle(dec)
	case "SELECT", "EXAMINE":
		err = c.handleSelect(dec, name == "EXAMINE")
	case "CLOSE", "UNSELECT":
		err = c.handleUnselect(dec, name == "CLOSE")
	case "APPEND":
		err = c.handleAppend(tag, dec)
		sendOK = false
	case "FETCH", "UID FETCH":
		err = c.handleFetch(dec, numKind)
	case "EXPUNGE":
		err = c.handleExpunge(dec)
	case "UID EXPUNGE":
		err = c.handleUIDExpunge(dec)
	case "STORE", "UID STORE":
		err = c.handleStore(dec, numKind)
	case "COPY", "UID COPY":
		err = c.handleCopy(tag, dec, numKind)
		sendOK = false
	case "MOVE", "UID MOVE":
		err = c.handleMove(dec, numKind)
	case "SEARCH", "UID SEARCH":
		err = c.handleSearch(tag, dec, numKind)
	default:
		discardLine(dec)
		err = &imap.Error{
			Type: imap.StatusResponseTypeBad,
			Text: "Unknown command",
		}
	}

	var (
		resp   *imap.StatusResponse
		decErr *imapwire.DecoderExpectError
	)
	if imapErr, ok := err.(*imap.Error); ok {
		resp = (*imap.StatusResponse)(imapErr)
	} else if errors.As(err, &decErr) {
		discardLine(dec)
		resp = &imap.StatusResponse{
			Type: imap.StatusResponseTypeBad,
			Code: imap.ResponseCodeClientBug,
			Text: "Syntax error: " + decErr.Message,
		}
	} else if err != nil {
		c.server.logger().Printf("handling %v command: %v", name, err)
		resp = internalServerErrorResp
	} else {
		if !sendOK {
			return nil
		}
		resp = &imap.StatusResponse{
			Type: imap.StatusResponseTypeOK,
			Text: fmt.Sprintf("%v completed", name),
		}
	}
	return c.writeStatusResp(tag, resp)
}

func (c *conn) handleNoop(dec *imapwire.Decoder) error {
	if !dec.ExpectCRLF() {
		return dec.Err()
	}
	return nil
}

func (c *conn) handleLogout(dec *imapwire.Decoder) error {
	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	c.state = imap.ConnStateLogout

	return c.writeStatusResp("", &imap.StatusResponse{
		Type: imap.StatusResponseTypeBye,
		Text: "Logging out",
	})
}

func (c *conn) handleCreate(dec *imapwire.Decoder) error {
	var name string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&name) || !dec.ExpectCRLF() {
		return dec.Err()
	}
	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}
	return c.session.Create(name)
}

func (c *conn) handleDelete(dec *imapwire.Decoder) error {
	var name string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&name) || !dec.ExpectCRLF() {
		return dec.Err()
	}
	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}
	return c.session.Delete(name)
}

func (c *conn) handleRename(dec *imapwire.Decoder) error {
	var oldName, newName string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&oldName) || !dec.ExpectSP() || !dec.ExpectMailbox(&newName) || !dec.ExpectCRLF() {
		return dec.Err()
	}
	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}
	return c.session.Rename(oldName, newName)
}

func (c *conn) handleSubscribe(dec *imapwire.Decoder) error {
	var name string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&name) || !dec.ExpectCRLF() {
		return dec.Err()
	}
	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}
	return c.session.Subscribe(name)
}

func (c *conn) handleUnsubscribe(dec *imapwire.Decoder) error {
	var name string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&name) || !dec.ExpectCRLF() {
		return dec.Err()
	}
	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}
	return c.session.Unsubscribe(name)
}

func (c *conn) checkBufferedLiteral(size int64, nonSync bool) error {
	if size > 4096 {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeTooBig,
			Text: "Literals are limited to 4096 bytes for this command",
		}
	}

	return c.acceptLiteral(size, nonSync)
}

func (c *conn) acceptLiteral(size int64, nonSync bool) error {
	if nonSync && size > 4096 {
		return &imap.Error{
			Type: imap.StatusResponseTypeBad,
			Text: "Non-synchronizing literals are limited to 4096 bytes",
		}
	}

	if nonSync {
		return nil
	}

	enc := newResponseEncoder(c)
	defer enc.end()
	return writeContReq(enc.Encoder, "Ready for literal data")
}

func (c *conn) canAuth() bool {
	if c.state != imap.ConnStateNotAuthenticated {
		return false
	}
	_, isTLS := c.conn.(*tls.Conn)
	return isTLS || c.server.InsecureAuth
}

func (c *conn) writeStatusResp(tag string, statusResp *imap.StatusResponse) error {
	enc := newResponseEncoder(c)
	defer enc.end()

	if tag == "" {
		tag = "*"
	}
	enc.Atom(tag).SP().Atom(string(statusResp.Type)).SP()
	if statusResp.Code != "" {
		enc.Atom(fmt.Sprintf("[%v]", statusResp.Code)).SP()
	}
	enc.Text(statusResp.Text)
	return enc.CRLF()
}

func (c *conn) writeGreeting() error {
	enc := newResponseEncoder(c)
	defer enc.end()

	enc.Atom("*").SP().Atom("OK").SP().Special('[').Atom("CAPABILITY")
	for _, c := range c.availableCaps() {
		enc.SP().Atom(string(c))
	}
	enc.Special(']').SP().Text("IMAP4rev2 server ready")
	return enc.CRLF()
}

func (c *conn) checkState(state imap.ConnState) error {
	if state == imap.ConnStateAuthenticated && c.state == imap.ConnStateSelected {
		return nil
	}
	if c.state != state {
		return newClientBugError(fmt.Sprintf("This command is only valid in the %s state", state))
	}
	return nil
}

func (c *conn) setReadTimeout(dur time.Duration) {
	if dur > 0 {
		c.conn.SetReadDeadline(time.Now().Add(dur))
	} else {
		c.conn.SetReadDeadline(time.Time{})
	}
}

func (c *conn) setWriteTimeout(dur time.Duration) {
	if dur > 0 {
		c.conn.SetWriteDeadline(time.Now().Add(dur))
	} else {
		c.conn.SetWriteDeadline(time.Time{})
	}
}

type responseEncoder struct {
	*imapwire.Encoder
	conn *conn
}

func newResponseEncoder(conn *conn) *responseEncoder {
	conn.mutex.Lock()
	quotedUTF8 := conn.enabled.Has(imap.CapIMAP4rev2)
	conn.mutex.Unlock()

	wireEnc := imapwire.NewEncoder(conn.bw, imapwire.ConnSideServer)
	wireEnc.QuotedUTF8 = quotedUTF8

	conn.encMutex.Lock() // released by responseEncoder.end
	conn.setWriteTimeout(respWriteTimeout)
	return &responseEncoder{
		Encoder: wireEnc,
		conn:    conn,
	}
}

func (enc *responseEncoder) end() {
	if enc.Encoder == nil {
		panic("imapserver: responseEncoder.end called twice")
	}
	enc.Encoder = nil
	enc.conn.setWriteTimeout(0)
	enc.conn.encMutex.Unlock()
}

func (enc *responseEncoder) Literal(size int64) io.WriteCloser {
	enc.conn.setWriteTimeout(literalWriteTimeout)
	return literalWriter{
		WriteCloser: enc.Encoder.Literal(size, nil),
		conn:        enc.conn,
	}
}

type literalWriter struct {
	io.WriteCloser
	conn *conn
}

func (lw literalWriter) Close() error {
	lw.conn.setWriteTimeout(respWriteTimeout)
	return lw.WriteCloser.Close()
}

func discardLine(dec *imapwire.Decoder) {
	var text string
	dec.Text(&text)
	dec.CRLF()
}

func writeContReq(enc *imapwire.Encoder, text string) error {
	return enc.Atom("+").SP().Text(text).CRLF()
}

func newClientBugError(text string) error {
	return &imap.Error{
		Type: imap.StatusResponseTypeBad,
		Code: imap.ResponseCodeClientBug,
		Text: text,
	}
}
