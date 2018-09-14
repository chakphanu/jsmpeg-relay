package main

import (
	"context"
	"flag"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"time"

	gctx "github.com/gorilla/context"
	"github.com/gorilla/mux"
	"github.com/numb3r3/jsmpeg-relay/log"
	"github.com/numb3r3/jsmpeg-relay/pubsub"
	"github.com/numb3r3/jsmpeg-relay/websocket"
)

//Broker default
var broker = pubsub.NewBroker()

var useStreamKey *bool

func publishHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	appName := vars["app_name"]
	streamKey := ""
	if *useStreamKey {
		streamKey = vars["stream_key"]
	}

	// logging.Infof("publish stream %v / %v", app_name, stream_key)
	if r.Body != nil {
		logging.Debugf("publishing stream %v / %v from %v", appName, streamKey, r.RemoteAddr)

		buf := make([]byte, 1024*1024)
		for {
			n, err := r.Body.Read(buf)
			if err != nil {
				logging.Error("[stream][recv] error:", err)
				return
			}
			if n > 0 {
				// logging.Info("broadcast stream")
				if *useStreamKey {
					broker.Broadcast(buf[:n], appName+"/"+streamKey)
				} else {
					broker.Broadcast(buf[:n], appName)
				}

			}
		}

	}
	defer r.Body.Close()
	w.WriteHeader(200)
	flusher := w.(http.Flusher)
	flusher.Flush()

}

func playHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	appName := vars["app_name"]
	streamKey := ""
	if *useStreamKey {
		streamKey = vars["stream_key"]
	}

	logging.Infof("play stream %v / %v", appName, streamKey)

	// TODO: identify unique connection by the same peer
	// addrStr := fmt.Sprintf("%p", &conn)
	// keyWord := conn.RemoteAddr().String() + conn.RemoteAddr().Network() + addrStr

	c, ok := websocket.TryUpgrade(w, r)
	if ok != true {
		logging.Error("[ws] upgrade failed")
		return
	}
	// cleanup on server side
	// defer c.Close()

	subscriber, err := broker.Attach()
	if err != nil {
		c.Close()
		logging.Error("subscribe error: ", err)
		return
	}

	// defer broker.Detach(subscriber)
	defer func() {
		logging.Debug("websocket closed: to unsubscribe")
		broker.Detach(subscriber)
		c.Close()
	}()

	logging.Info("client remote addr: ", c.RemoteAddr())
	if *useStreamKey {
		broker.Subscribe(subscriber, appName+"/"+streamKey)
	} else {
		broker.Subscribe(subscriber, appName)
	}

	for {
		select {
		// case <- c.Closing():
		//     logging.Info("websocket closed: to unsubscribe")
		//     broker.Detach(subscriber)
		case <-c.Closing():
			logging.Debug("received closeing signal, to close the websocket")
			return
		case <-subscriber.Closing():
			logging.Debug("subscriber destroyed")
			return
		case msg := <-subscriber.GetMessages():
			// logging.Info("[stream][send]")
			data := msg.GetData()
			if _, err := c.Write(data); err != nil {
				// logging.Debug("to unsubscribe")
				// broker.Detach(subscriber)
				logging.Error("websockt write mesage error: ", err)
				return
			}
		}
	}
	return
}

func main() {
	var listenAddr = flag.String("l", "0.0.0.0:8080", "")
	var debugKey = flag.String("d", "", "")
	var sslCert = flag.String("sslcert", "", "")
	var sslPriv = flag.String("sslpriv", "", "")
	useStreamKey = flag.Bool("k", true, "-k=true or -k=false")
	var wait time.Duration
	flag.DurationVar(&wait, "graceful-timeout", time.Second*15, "the duration for which the server gracefully wait for existing connections to finish - e.g. 15s or 1m")
	flag.Parse()

	logging.Info("start ws-relay ....")
	logging.Infof("server listen @ %v", *listenAddr)
	if *sslCert != "" && *sslPriv != "" {
		logging.Infof("SSL enable")
	} else {
		logging.Infof("SSL disable")
	}

	r := mux.NewRouter()
	if *useStreamKey {
		logging.Infof("StreamKey enable")
		r.HandleFunc("/publish/{app_name}/{stream_key}", publishHandler).Methods("POST")
		r.HandleFunc("/play/{app_name}/{stream_key}", playHandler)
	} else {
		logging.Infof("StreamKey disable")
		r.HandleFunc("/publish/{app_name}", publishHandler).Methods("POST")
		r.HandleFunc("/play/{app_name}", playHandler)
	}

	var debugPath = "/debug/"
	if *debugKey != "" {
		debugPath = debugPath + *debugKey + "/"
		logging.Infof("debug path = %v", debugPath)
	}
	r.HandleFunc(debugPath+"pprof/", pprof.Index)
	r.HandleFunc(debugPath+"pprof/cmdline", pprof.Cmdline)
	r.HandleFunc(debugPath+"pprof/profile", pprof.Profile)
	r.HandleFunc(debugPath+"pprof/symbol", pprof.Symbol)
	r.HandleFunc(debugPath+"pprof/trace", pprof.Trace)

	// Manually add support for paths linked to by index page at /debug/pprof/
	r.Handle(debugPath+"pprof/goroutine", pprof.Handler("goroutine"))
	r.Handle(debugPath+"pprof/heap", pprof.Handler("heap"))
	r.Handle(debugPath+"pprof/threadcreate", pprof.Handler("threadcreate"))
	r.Handle(debugPath+"pprof/block", pprof.Handler("block"))

	srv := &http.Server{
		Addr: *listenAddr,
		// Good practice to set timeouts to avoid Slowloris attacks.
		WriteTimeout: time.Second * 0,
		ReadTimeout:  time.Second * 0,
		IdleTimeout:  time.Second * 60,
		// Important Note: If you aren't using gorilla/mux, you need to wrap your handlers with
		// context.ClearHandler as or else you will leak memory! An easy way to do this is to
		// wrap the top-level mux when calling http.ListenAndServe:
		Handler: gctx.ClearHandler(r), // Pass our instance of gorilla/mux in.
	}

	// Run our server in a goroutine so that it doesn't block.
	go func() {
		if *sslCert != "" && *sslPriv != "" {
			if err := srv.ListenAndServeTLS(*sslCert, *sslPriv); err != nil {
				logging.Error("server(SSL) listen error:", err)
			}
		} else {
			if err := srv.ListenAndServe(); err != nil {
				logging.Error("server listen error:", err)
			}
		}

	}()

	c := make(chan os.Signal, 1)
	// We'll accept graceful shutdowns when quit via SIGINT (Ctrl+C)
	// SIGKILL, SIGQUIT or SIGTERM (Ctrl+/) will not be caught.
	signal.Notify(c, os.Interrupt)

	// Block until we receive our signal.
	<-c

	// Create a deadline to wait for.
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	// Doesn't block if no connections, but will otherwise wait
	// until the timeout deadline.
	srv.Shutdown(ctx)
	// Optionally, you could run srv.Shutdown in a goroutine and block on
	// <-ctx.Done() if your application should wait for other services
	// to finalize based on context cancellation.
	logging.Info("shutting down")
	os.Exit(0)

}
