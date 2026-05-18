package server

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

func SocketPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/.recon/grecon.sock"
	}
	return filepath.Join(home, ".recon", "grecon.sock")
}

func SerializeSessions(sessions []*Session) []byte {
	if sessions == nil {
		sessions = []*Session{}
	}
	data, err := json.Marshal(sessions)
	if err != nil {
		data = []byte("[]")
	}
	buf := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(buf[:4], uint32(len(data)))
	copy(buf[4:], data)
	return buf
}

func RunServer() {
	path := SocketPath()
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.Remove(path)

	prev := make(map[string]*Session)
	initial := discoverTmuxSessions(prev)
	for _, s := range initial {
		prev[s.SessionID] = s
	}
	AttachSummaries(initial)

	pw := NewPaneWatcher()
	defer pw.Stop()
	syncPaneWatcher(pw, initial)

	var mu sync.Mutex
	data := SerializeSessions(initial)
	var subs []net.Conn

	listener, err := net.Listen("unix", path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to bind %s: %v\n", path, err)
		os.Exit(1)
	}
	defer listener.Close()

	fmt.Fprintf(os.Stderr, "grecon server listening on %s\n", path)

	go listenCommands()

	broadcast := func(sessions []*Session) {
		for _, s := range sessions {
			if status, ok := pw.GetStatus(s.TmuxSession); ok {
				s.Status = debounceStatus(s.SessionID, status)
			}
		}

		newData := SerializeSessions(sessions)

		mu.Lock()
		data = newData
		var alive []net.Conn
		for _, conn := range subs {
			conn.SetWriteDeadline(time.Now().Add(time.Second))
			if _, err := conn.Write(newData); err != nil {
				conn.Close()
			} else {
				alive = append(alive, conn)
			}
		}
		subs = alive
		mu.Unlock()
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "PANIC in poll goroutine: %v\n", r)
			}
		}()
		sessions := initial
		pollCount := uint64(0)
		discoverTicker := time.NewTicker(500 * time.Millisecond)
		defer discoverTicker.Stop()

		for {
			select {
			case <-pw.Notify():
				broadcast(sessions)

			case <-discoverTicker.C:
				pollCount++
				pollStart := time.Now()
				sessions = discoverTmuxSessions(prev)
				pollMs := time.Since(pollStart).Milliseconds()

				prev = make(map[string]*Session)
				for _, s := range sessions {
					prev[s.SessionID] = s
				}

				AttachSummaries(sessions)
				syncPaneWatcher(pw, sessions)

				fmt.Printf("poll #%d: discover=%dms sessions=%d\n",
					pollCount, pollMs, len(sessions))

				broadcast(sessions)
			}
		}
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		mu.Lock()
		snapshot := make([]byte, len(data))
		copy(snapshot, data)
		mu.Unlock()

		conn.SetWriteDeadline(time.Now().Add(time.Second))
		if _, err := conn.Write(snapshot); err != nil {
			conn.Close()
			continue
		}

		mu.Lock()
		subs = append(subs, conn)
		mu.Unlock()
	}
}

func TryFetch() []*Session {
	path := SocketPath()
	conn, err := net.DialTimeout("unix", path, 500*time.Millisecond)
	if err != nil {
		return nil
	}
	defer conn.Close()
	return readFrame(conn, 500*time.Millisecond)
}

func RequireFetch() ([]*Session, error) {
	sessions := TryFetch()
	if sessions != nil {
		return sessions, nil
	}
	return nil, fmt.Errorf("grecon server is not running. Start it with: grecon server")
}

func Subscribe(stop <-chan struct{}) <-chan []*Session {
	ch := make(chan []*Session, 1)
	go func() {
		defer close(ch)
		for {
			select {
			case <-stop:
				return
			default:
			}
			conn, err := net.DialTimeout("unix", SocketPath(), 500*time.Millisecond)
			if err != nil {
				select {
				case <-stop:
					return
				case <-time.After(time.Second):
					continue
				}
			}
			readFramesLoop(conn, ch, stop)
			conn.Close()
		}
	}()
	return ch
}

func readFramesLoop(conn net.Conn, ch chan []*Session, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		sessions := readFrame(conn, 5*time.Second)
		if sessions == nil {
			return
		}
		select {
		case ch <- sessions:
		default:
			<-ch
			ch <- sessions
		}
	}
}

func readFrame(conn net.Conn, deadline time.Duration) []*Session {
	conn.SetReadDeadline(time.Now().Add(deadline))
	var lenBuf [4]byte
	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		return nil
	}
	length := binary.BigEndian.Uint32(lenBuf[:])
	if length == 0 || length > 10_000_000 {
		return nil
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return nil
	}
	var sessions []*Session
	if json.Unmarshal(buf, &sessions) != nil {
		return nil
	}
	return sessions
}

func discoverTmuxSessions(prev map[string]*Session) []*Session {
	all := DiscoverSessions(prev)
	var sessions []*Session
	for _, s := range all {
		if s.TmuxSession != "" {
			sessions = append(sessions, s)
		}
	}
	return sessions
}

func syncPaneWatcher(pw *PaneWatcher, sessions []*Session) {
	var names []string
	for _, s := range sessions {
		if s.TmuxSession != "" {
			names = append(names, s.TmuxSession)
		}
	}
	pw.Sync(names)
}
