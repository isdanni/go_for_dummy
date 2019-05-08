package debug

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

type Debugger struct {
	// Address is the address of the web server. It is 127.0.0.1 by default.
	Address         string
	initialized     bool
	CurrentRequests map[uint32]requestInfo
	RequestLog      []requestInfo
}
