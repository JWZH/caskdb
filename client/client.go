package client

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"time"
)

var MaxFreeConns = 20
var ConnectTimeout time.Duration = time.Millisecond * 300
var ReadTimeout time.Duration = time.Millisecond * 2000
var WriteTimeout time.Duration = time.Millisecond * 2000

type Client struct {
	Addr     string
	nextDial time.Time
	conns    chan net.Conn
}

func NewClient(addr string) *Client {
	host := &Client{Addr: addr}
	host.conns = make(chan net.Conn, MaxFreeConns)
	return host
}

// Given a string of the form "host", "host:port", or "[ipv6::address]:port",
// return true if the string includes a port.
func hasPort(s string) bool { return strings.LastIndex(s, ":") > strings.LastIndex(s, "]") }

func (host *Client) Close() {
	if host.conns == nil {
		return
	}
	ch := host.conns
	host.conns = nil
	close(ch)

	for c, closed := <-ch; closed; {
		c.Close()
	}
}

func (host *Client) createConn() (net.Conn, error) {
	now := time.Now()
	if host.nextDial.After(now) {
		return nil, errors.New("wait for retry")
	}

	addr := host.Addr
	if !hasPort(addr) {
		addr = addr + ":11211"
	}
	conn, err := net.DialTimeout("tcp", addr, ConnectTimeout)
	if err != nil {
		host.nextDial = now.Add(time.Second * 10)
		return nil, err
	}
	return conn, nil
}

func (host *Client) getConn() (c net.Conn, err error) {
	if host.conns == nil {
		return nil, errors.New("host closed")
	}
	select {
	case c = <-host.conns:
	default:
		c, err = host.createConn()
	}
	return
}

func (host *Client) releaseConn(conn net.Conn) {
	if host.conns == nil {
		conn.Close()
		return
	}
	select {
	case host.conns <- conn:
	default:
		conn.Close()
	}
}

func (host *Client) execute(req *Request) (resp *Response, err error) {
	var conn net.Conn
	conn, err = host.getConn()
	if err != nil {
		return
	}

	err = req.Write(conn)
	if err != nil {
		log.Print("write request failed:", err)
		conn.Close()
		return
	}

	resp = new(Response)
	if req.NoReply {
		host.releaseConn(conn)
		resp.status = "STORED"
		return
	}

	reader := bufio.NewReader(conn)
	err = resp.Read(reader)
	if err != nil {
		log.Print("read response failed:", err)
		conn.Close()
		return
	}

	if err := req.Check(resp); err != nil {
		log.Print("unexpected response", req, resp, err)
		conn.Close()
		return nil, err
	}

	host.releaseConn(conn)
	return
}

func (host *Client) executeWithTimeout(req *Request, timeout time.Duration) (resp *Response, err error) {
	done := make(chan bool, 1)
	go func() {
		resp, err = host.execute(req)
		done <- true
	}()

	select {
	case <-done:
	case <-time.After(timeout):
		err = fmt.Errorf("request %v timeout", req)
	}
	return
}

func (host *Client) Get(key string) (*Item, error) {
	req := &Request{Cmd: "get", Key: key}
	resp, err := host.executeWithTimeout(req, ReadTimeout)
	if err != nil {
		return nil, err
	}
	item, _ := resp.items[key]
	return item, nil
}

func (host *Client) store(cmd string, key string, item *Item, noreply bool) (bool, error) {
	req := &Request{Cmd: cmd, Key: key, Item: item, NoReply: noreply}
	resp, err := host.executeWithTimeout(req, WriteTimeout)
	return err == nil && resp.status == "STORED", err
}

func (host *Client) Set(key string, value []byte) (bool, error) {
	return host.store("set", key, &Item{Body: value}, false)
}

func (host *Client) FlushAll() {
	req := &Request{Cmd: "flush_all"}
	host.execute(req)
}

func (host *Client) Delete(key string) (bool, error) {
	req := &Request{Cmd: "delete", Key: key}
	resp, err := host.execute(req)
	return err == nil && resp.status == "DELETED", err
}

func (host *Client) Stat() (map[string]string, error) {
	req := &Request{Cmd: "stats"}
	resp, err := host.execute(req)
	if err != nil {
		return nil, err
	}
	st := make(map[string]string)
	for key, item := range resp.items {
		st[key] = string(item.Body)
	}
	return st, nil
}

func (host *Client) Len() int {
	return 0
}
