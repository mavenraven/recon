package server

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Command struct {
	Type        string `json:"type"`
	TmuxSession string `json:"tmux_session,omitempty"`
	OriginalCWD string `json:"original_cwd,omitempty"`
}

func CommandSocketPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/.recon/grecon-cmd.sock"
	}
	return filepath.Join(home, ".recon", "grecon-cmd.sock")
}

func SendCommand(cmd Command) error {
	data, err := json.Marshal(cmd)
	if err != nil {
		return err
	}

	conn, err := net.DialTimeout("unix", CommandSocketPath(), 500*time.Millisecond)
	if err != nil {
		return fmt.Errorf("server not running: %w", err)
	}
	defer conn.Close()

	buf := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(buf[:4], uint32(len(data)))
	copy(buf[4:], data)

	conn.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := conn.Write(buf); err != nil {
		return err
	}

	conn.SetReadDeadline(time.Now().Add(time.Second))
	var ack [1]byte
	if _, err := io.ReadFull(conn, ack[:]); err != nil {
		return err
	}
	return nil
}

func listenCommands() {
	path := CommandSocketPath()
	os.Remove(path)

	listener, err := net.Listen("unix", path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to bind command socket %s: %v\n", path, err)
		return
	}
	defer listener.Close()

	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		go handleCommand(conn)
	}
}

func handleCommand(conn net.Conn) {
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(time.Second))
	var lenBuf [4]byte
	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		return
	}
	length := binary.BigEndian.Uint32(lenBuf[:])
	if length == 0 || length > 1_000_000 {
		return
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return
	}

	var cmd Command
	if json.Unmarshal(buf, &cmd) != nil {
		return
	}

	conn.SetWriteDeadline(time.Now().Add(time.Second))
	conn.Write([]byte{0x01})

	switch cmd.Type {
	case "fix-default-path":
		go fixDefaultPath(cmd.TmuxSession, cmd.OriginalCWD)
	}
}

func fixDefaultPath(tmuxSession, originalCWD string) {
	worktreeDir := filepath.Join(originalCWD, ".claude", "worktrees")
	for i := 0; i < 30; i++ {
		time.Sleep(500 * time.Millisecond)
		out, err := exec.Command("tmux", "display-message", "-t", tmuxSession, "-p", "#{pane_current_path}").Output()
		if err != nil {
			continue
		}
		panePath := strings.TrimSpace(string(out))
		if panePath != "" && panePath != originalCWD && strings.HasPrefix(panePath, worktreeDir) {
			exec.Command("tmux", "attach-session", "-t", tmuxSession, "-c", panePath).Run()
			return
		}
	}
}
