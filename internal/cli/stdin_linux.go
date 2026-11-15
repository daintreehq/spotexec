package cli

import "golang.org/x/sys/unix"

// fionread is FIONREAD (TIOCINQ on Linux): the bytes waiting in a pipe or
// socket.
const fionread = unix.TIOCINQ
