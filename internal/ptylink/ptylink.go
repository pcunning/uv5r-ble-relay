// Package ptylink opens a pseudo-terminal pair and optionally exposes the
// slave end via a stable symlink so a serial-port consumer (e.g. CHIRP) can
// connect.
package ptylink

import (
	"fmt"
	"os"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// PTY owns a master/slave pair and an optional symlink pointing at the slave.
type PTY struct {
	master *os.File
	slave  *os.File
	link   string
}

// Open creates a new pty pair. If symlinkPath is non-empty, an existing entry
// at that path is removed and a symlink to the slave device is created.
func Open(symlinkPath string) (*PTY, error) {
	master, slave, err := pty.Open()
	if err != nil {
		return nil, fmt.Errorf("pty.Open: %w", err)
	}
	// Put the slave end into raw mode so the line discipline doesn't echo,
	// translate CR/LF, or buffer until newline. CHIRP needs a transparent
	// byte stream.
	if _, err := term.MakeRaw(int(slave.Fd())); err != nil {
		_ = master.Close()
		_ = slave.Close()
		return nil, fmt.Errorf("term.MakeRaw: %w", err)
	}
	// Set the master fd non-blocking and re-wrap it so the Go runtime
	// registers it with the netpoller. Without this, SetReadDeadline returns
	// "file type does not support deadline" on a PTY master, and there is no
	// reliable way to interrupt a blocked Read at shutdown on macOS.
	if err := unix.SetNonblock(int(master.Fd()), true); err != nil {
		_ = master.Close()
		_ = slave.Close()
		return nil, fmt.Errorf("set master non-blocking: %w", err)
	}
	masterPollable := os.NewFile(master.Fd(), master.Name())
	// We intentionally do not call master.Close() here; masterPollable shares
	// the fd. Closing masterPollable releases the descriptor.
	_ = master
	p := &PTY{master: masterPollable, slave: slave, link: symlinkPath}
	if symlinkPath != "" {
		_ = os.Remove(symlinkPath)
		if err := os.Symlink(slave.Name(), symlinkPath); err != nil {
			_ = master.Close()
			_ = slave.Close()
			return nil, fmt.Errorf("symlink %s: %w", symlinkPath, err)
		}
	}
	return p, nil
}

// Master returns the master *os.File. Reads on the master receive bytes
// written to the slave (and vice versa).
func (p *PTY) Master() *os.File { return p.master }

// SlavePath returns the device path of the slave end (e.g. /dev/ttys003).
func (p *PTY) SlavePath() string { return p.slave.Name() }

// Close releases both ends of the pty pair and removes the symlink if any.
func (p *PTY) Close() error {
	var first error
	if err := p.master.Close(); err != nil {
		first = err
	}
	if err := p.slave.Close(); err != nil && first == nil {
		first = err
	}
	if p.link != "" {
		if err := os.Remove(p.link); err != nil && !os.IsNotExist(err) && first == nil {
			first = err
		}
	}
	return first
}
