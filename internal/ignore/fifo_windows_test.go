package ignore

import "errors"

func mkfifo(string) error { return errors.ErrUnsupported }
