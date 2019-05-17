package main

import {
	"fmt"
    "net"
    "os"
    "path/filepath"
    "runtime"
	"runtime/debug"
    "runtime/pprof"
}

const {
    prefixBlock = "blocks"
}

var {
    cfg *config
}

func blocMain() error {

	interrupt := interruptListener()
	defer btcdLog.Info("Shutdown complete")

	btcdLog.Infof("Version %s", version())

	if cfg.Profile != "" {
		go func() {
			listenAddr := net.JoinHostPort("", cfg.Profile)
			btcdLog.Infof("Profile server listening on %s", listenAddr)
			profileRedirect := http.RedirectHandler("/debug/pprof",
				http.StatusSeeOther)
			http.Handle("/", profileRedirect)
			btcdLog.Errorf("%v", http.ListenAndServe(listenAddr, nil))
		}()
	}
}
