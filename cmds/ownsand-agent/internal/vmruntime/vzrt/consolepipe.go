// SPDX-License-Identifier: FSL-1.1-ALv2

package vzrt

import "io"

// drainConsole copies the guest console bytes from r to w until EOF or a read
// error, and then closes r when it is an io.Closer. It is the only code
// between the console pipe and the bounded log writer: the writer consumes
// every byte (see vmruntime.CappedLog), so the pipe never fills and the
// guest never blocks on its console. It is tag-free so that Linux CI can test
// it with an os.Pipe.
func drainConsole(r io.Reader, w io.Writer) {
	_, _ = io.Copy(w, r)
	if c, ok := r.(io.Closer); ok {
		_ = c.Close()
	}
}
