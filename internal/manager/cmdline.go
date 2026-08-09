package manager

import "strings"

// A shell has no title of its own the way an agent does, so what a shell
// session is doing has to be read off the only thing the manager can see: the
// keystrokes it forwards to the PTY. cmdLine reassembles the line being typed
// at the prompt and reports it when it is submitted.
//
// This is an approximation, and cannot be anything else. The manager sees what
// was typed but not what the shell made of it: a completion the shell filled
// in was never typed, and a full-screen program started from the prompt takes
// keystrokes that are not a command line at all. It is right for the ordinary
// case — someone typing a command and pressing Enter — which is enough for a
// title.
type cmdLine struct {
	buf   []rune
	state escState
}

// escState tracks an escape sequence being skipped, which can arrive split
// across writes.
type escState int

const (
	escNone escState = iota
	// escStart is the rune after ESC, which says what kind of sequence it is.
	escStart
	// escSeq is inside a CSI or SS3 sequence, which runs to its final byte.
	escSeq
)

const (
	// maxCmdLine bounds the line being assembled. Nothing shortens a line the
	// manager cannot follow, so a session left holding a key must not be able
	// to grow this without limit.
	maxCmdLine = 4096
	// maxCommandTitle bounds what is shown, matching an agent's own title: a
	// command lands in the same place in the sidebar.
	maxCommandTitle = maxAgentTitle
)

// feed adds keystrokes to the line, and returns the last command they
// submitted — empty when they submitted none.
func (c *cmdLine) feed(p []byte) string {
	var submitted string
	for _, r := range string(p) {
		if cmd := c.step(r); cmd != "" {
			submitted = cmd
		}
	}
	return submitted
}

// reset drops a half-typed line, for a session that is starting over.
func (c *cmdLine) reset() {
	c.buf = c.buf[:0]
	c.state = escNone
}

// step applies one keystroke, returning the command it submitted if it was the
// Enter that ended one.
func (c *cmdLine) step(r rune) string {
	switch c.state {
	case escStart:
		// A CSI ("\x1b[") or SS3 ("\x1bO") sequence runs to its final byte;
		// after any other rune the sequence is already over, as with Alt-key.
		if r == '[' || r == 'O' {
			c.state = escSeq
		} else {
			c.state = escNone
		}
		return ""
	case escSeq:
		if r >= 0x40 && r <= 0x7e {
			c.state = escNone
		}
		return ""
	}

	switch {
	case r == '\r' || r == '\n':
		cmd := strings.TrimSpace(string(c.buf))
		c.buf = c.buf[:0]
		return cmd
	case r == 0x1b:
		c.state = escStart
	case r == 0x7f || r == '\b':
		if n := len(c.buf); n > 0 {
			c.buf = c.buf[:n-1]
		}
	case r == 0x17: // Ctrl-W erases the word before the cursor
		c.buf = trimLastWord(c.buf)
	case r == 0x03 || r == 0x04 || r == 0x15: // Ctrl-C, Ctrl-D, Ctrl-U
		c.buf = c.buf[:0]
	case r < 0x20:
		// Every other control key does something to the line that cannot be
		// followed from input alone: Tab completes it, Ctrl-A moves inside it.
		// Ignoring them leaves what was actually typed, which is the best
		// available guess at the line.
	case len(c.buf) < maxCmdLine:
		c.buf = append(c.buf, r)
	}
	return ""
}

// trimLastWord drops the trailing run of spaces and the word before it.
func trimLastWord(buf []rune) []rune {
	i := len(buf)
	for i > 0 && buf[i-1] == ' ' {
		i--
	}
	for i > 0 && buf[i-1] != ' ' {
		i--
	}
	return buf[:i]
}
