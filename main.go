package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	var (
		listen = flag.String("listen", ":9398", "listen address for /metrics")
		dev    = flag.String("dev", "/dev/ttyUSB0", "serial device of the W6832 dongle")
		dump   = flag.Duration("dump", 0, "listen this long, print each push frame, and exit instead of serving")
		hex    = flag.Bool("hex", false, "with --dump: also print each frame as hex")
	)
	flag.Parse()

	e := NewE450(*dev)

	if *dump > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), *dump)
		defer cancel()
		e.onPush = func(h head, frame []byte) {
			var total float64
			for j := 0; j < 3; j++ {
				total += float64(h.currentMA[j]) / 1000 * float64(h.voltageV[j])
			}
			fmt.Printf("e450: I=%d/%d/%d mA  U=%d/%d/%d V  power=%.1f W\n",
				h.currentMA[0], h.currentMA[1], h.currentMA[2],
				h.voltageV[0], h.voltageV[1], h.voltageV[2], total)
			if *hex {
				fmt.Printf("    %x\n", frame)
			}
		}
		e.Run(ctx)
		return
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{Addr: *listen, Handler: mux}
	go func() {
		log.Printf("e450-collector: listening on %s", *listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()
	e.Run(context.Background())
}
