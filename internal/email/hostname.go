package email

import "os"

// osHostname is the indirection point that lets tests stub out
// os.Hostname() — used by smtp.go's hostnameOrDefault for the EHLO
// argument. Production always uses os.Hostname.
var osHostname = func() string {
	h, _ := os.Hostname()
	return h
}
