package socks

import (
	"context"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"golang.org/x/net/proxy"

	"github.com/xjasonlyu/tun2socks/common/dns"
	"github.com/xjasonlyu/tun2socks/common/log"
	"github.com/xjasonlyu/tun2socks/common/lsof"
	"github.com/xjasonlyu/tun2socks/common/stats"
	"github.com/xjasonlyu/tun2socks/core"
)

type tcpHandler struct {
	proxyHost string
	proxyPort uint16

	fakeDns       dns.FakeDns
	sessionStater stats.SessionStater
}

func NewTCPHandler(proxyHost string, proxyPort uint16, fakeDns dns.FakeDns, sessionStater stats.SessionStater) core.TCPConnHandler {
	return &tcpHandler{
		proxyHost:     proxyHost,
		proxyPort:     proxyPort,
		fakeDns:       fakeDns,
		sessionStater: sessionStater,
	}
}

func ctxCopy(ctx context.Context, dst, src net.Conn) (written int64, err error) {
	buf := core.NewBytes(core.BufSize)
	defer core.FreeBytes(buf)

	for {
		select {
		case <-ctx.Done():
			return written, err
		default:
		}
		src.SetReadDeadline(time.Now().Add(30*time.Second))
		nr, er := src.Read(buf)
		if nr > 0 {
			dst.SetWriteDeadline(time.Now().Add(30*time.Second))
			nw, ew := dst.Write(buf[0:nr])
			if nw > 0 {
				written += int64(nw)
			}
			if ew != nil {
				if ew, ok := ew.(net.Error); !ok || !ew.Timeout() {
					err = ew
					break
				}
			}
			if nr != nw {
				err = io.ErrShortWrite
				break
			}
		}
		if er != nil {
			if er, ok := er.(net.Error); !ok || !er.Timeout() {
				if er != io.EOF {
					err = er
				}
				break
			}
		}
	}
	return written, err
}

func (h *tcpHandler) relay(localConn, remoteConn net.Conn) {
	var once sync.Once
	closeOnce := func() {
		once.Do(func() {
			localConn.Close()
			remoteConn.Close()
		})
	}

	// Close
	defer closeOnce()

	up := make(chan struct{})
	down := make(chan struct{})

	// UpLink
	go func() {
		if _, err := io.Copy(remoteConn, localConn); err != nil {
			closeOnce()
		}
		up <- struct{}{}
	}()

	// DownLink
	go func() {
		if _, err := io.Copy(localConn, remoteConn); err != nil {
			closeOnce()
		}
		down <- struct{}{}
	}()

	select {
	case <-up: // Wait for Up Link done
	case <-down: // Wait for Down Link done
	}

	if h.sessionStater != nil {
		h.sessionStater.RemoveSession(localConn)
	}
}

func (h *tcpHandler) Handle(localConn net.Conn, target *net.TCPAddr) error {
	dialer, err := proxy.SOCKS5("tcp", core.ParseTCPAddr(h.proxyHost, h.proxyPort).String(), nil, nil)
	if err != nil {
		return err
	}

	// Replace with a domain name if target address IP is a fake IP.
	var targetHost = target.IP.String()
	if h.fakeDns != nil {
		if host, exist := h.fakeDns.IPToHost(target.IP); exist {
			targetHost = host
		}
	}

	targetAddr := net.JoinHostPort(targetHost, strconv.Itoa(target.Port))
	remoteConn, err := dialer.Dial(target.Network(), targetAddr)
	if err != nil {
		return err
	}

	var process string
	var sess *stats.Session
	if h.sessionStater != nil {
		// Get name of the process.
		localHost, localPortStr, _ := net.SplitHostPort(localConn.LocalAddr().String())
		localPortInt, _ := strconv.Atoi(localPortStr)
		process, err = lsof.GetCommandNameBySocket(target.Network(), localHost, uint16(localPortInt))
		if err != nil {
			process = "N/A"
		}

		sess = &stats.Session{
			ProcessName:   process,
			Network:       target.Network(),
			ClientAddr:    localConn.LocalAddr().String(),
			TargetAddr:    targetAddr,
			UploadBytes:   0,
			DownloadBytes: 0,
			SessionStart:  time.Now(),
		}
		h.sessionStater.AddSession(localConn, sess)

		remoteConn = stats.NewSessionConn(remoteConn, sess)
	}

	// set keepalive
	tcpKeepAlive(localConn)
	tcpKeepAlive(remoteConn)

	// relay connections
	go h.relay(localConn, remoteConn)

	log.Access(process, "proxy", target.Network(), localConn.LocalAddr().String(), targetAddr)
	return nil
}

func tcpKeepAlive(conn net.Conn) {
	if tcp, ok := conn.(*net.TCPConn); ok {
		tcp.SetKeepAlive(true)
		tcp.SetKeepAlivePeriod(30 * time.Second)
	}
}
