package cli

// fionread is FIONREAD, _IOR('f', 127, int) in <sys/filio.h>, which
// golang.org/x/sys/unix doesn't define for darwin: the bytes waiting in a
// pipe or socket.
const fionread = 0x4004667f
