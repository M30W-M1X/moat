package vm

import "io"

// isolatedWriter wraps an io.Writer in a unique non-*os.File pointer.
//
// Passing os.Stdout and os.Stderr directly to os/exec shares the parent's open
// file descriptions with Firecracker. When stdout and stderr share one open
// file description (for example under 2>&1 or on a PTY), Firecracker sets
// O_NONBLOCK on fd 1 during serial device setup. That flag leaks onto fd 2 and
// causes Rust eprintln! to panic with EAGAIN and abort with SIGABRT.
//
// Each isolatedWriter pointer is distinct, so os/exec creates separate kernel
// pipes for stdout and stderr. Setting O_NONBLOCK on fd 1 never affects fd 2
// or the parent's terminal.
type isolatedWriter struct {
	w io.Writer
}

func (iw *isolatedWriter) Write(p []byte) (int, error) {
	return iw.w.Write(p)
}

// isolateStdio returns two distinct non-*os.File writers that forward to
// stdout and stderr. os/exec gives each writer its own pipe.
func isolateStdio(stdout, stderr io.Writer) (io.Writer, io.Writer) {
	return &isolatedWriter{w: stdout}, &isolatedWriter{w: stderr}
}
