package podcast

import "syscall"

// unsupportedLinkErrors are CreateHardLink's answers on a volume without hard
// links: ERROR_INVALID_FUNCTION (1), ERROR_ACCESS_DENIED (5, FAT and some
// network shares), ERROR_NOT_SUPPORTED (50), ERROR_CALL_NOT_IMPLEMENTED (120)
// and ERROR_TOO_MANY_LINKS (1142).
var unsupportedLinkErrors = []error{syscall.Errno(1), syscall.Errno(5), syscall.Errno(50), syscall.Errno(120), syscall.Errno(1142)}
