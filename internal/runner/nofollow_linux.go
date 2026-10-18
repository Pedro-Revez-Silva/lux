package runner

import "syscall"

const oNoFollow = syscall.O_NOFOLLOW

// oNonBlock keeps an open of a FIFO planted in place of a file from
// blocking until a writer appears.
const oNonBlock = syscall.O_NONBLOCK
