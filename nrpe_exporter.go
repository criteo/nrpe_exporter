package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mitchellh/hashstructure"

	"sync"

	"github.com/criteo/nrpe_exporter/nrpe"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/version"
	l "github.com/sirupsen/logrus"
)

const (
	// NAMESPACE of the prometheus metrics name
	NAMESPACE = "nrpe"

	DEFAULT_NRPE_PORT = "5666"
)

var (
	listenAddr = flag.String("listen-addr", "0.0.0.0", "The address on which the daemon should listen")
	port       = flag.Int("port", 9235, "The port on which the daemon should bind")
	timeout    = flag.Duration("timeout", 10*time.Minute, "Maximum time to wait for a response.")
	verbose    = flag.Bool("v", false, "Trigger more verbose output")

	runningQueries = sync.Map{}
)

// Command represent a Nagios's Command (I.E: check_foo!bar!foobar)
type Command struct {
	Command string
	Args    []string
}

// NewCommand returns a new Command object
func NewCommand(command string, args []string) Command {
	return Command{Command: command, Args: args}
}

// Query is an instance of a Command being run
type Query struct {
	*prometheus.Registry
	Command      Command
	Target       string
	lock         bool
	hash         uint64
	wg           sync.WaitGroup
	o            sync.Once
	err			 error
}

// NewQuery returns a new Query object
func NewQuery(target string, command string, args []string, lock bool) Query {

	return Query{
		Command:  NewCommand(command, args),
		Target:   target,
		lock:     lock,
		Registry: prometheus.NewRegistry(),
	}
}

// GetQuery return a query instance, and give the locked run their shared Query instance
func GetQuery(target string, command string, args []string, lock bool) (*Query, error) {
	ret := NewQuery(target, command, args, lock)
	if lock {
		hash, err := hashstructure.Hash(ret, nil)
		if err != nil {
			return nil, err
		}
		ret.hash = hash
		q, _ := runningQueries.LoadOrStore(hash, &ret)
		return q.(*Query), nil
	}

	return &ret, nil
}

// CommandResult type describing the result of Command against nrpe-server
type CommandResult struct {
	commandDuration float64
	statusOk        float64
	result          *nrpe.CommandResult
}

// Run Runs the query against the daemon and return success and error if any
func (q *Query) Run() {
	if q.lock {
		defer runningQueries.Delete(q.hash)
	}

	cmd := nrpe.NewCommand(q.Command.Command, q.Command.Args...)
	cmdLine := cmd.ToStatusLine()
	duration := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   NAMESPACE,
		Subsystem:   "command",
		Name:        "duration",
		Help:        "The time it took to run the command.",
		ConstLabels: prometheus.Labels{"command": cmdLine},
	})
	status := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   NAMESPACE,
		Subsystem:   "command",
		Name:        "status",
		Help:        "The status code returned by the server.",
		ConstLabels: prometheus.Labels{"command": cmdLine},
	})
	locked := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   NAMESPACE,
		Subsystem:   "command",
		Name:        "locked_query",
		Help:        "1 if the query have shared its lock",
		ConstLabels: prometheus.Labels{"command": cmdLine},
	})
	perfDatas := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace:   NAMESPACE,
		Subsystem:   "command",
		Name:        "perf_data",
		Help:        "1 if the query has been locked by another",
		ConstLabels: prometheus.Labels{"command": cmdLine},
	}, []string{"perfdata_key"})

	// Register the metrics
	q.MustRegister(duration, status, locked, perfDatas)

	// Put the duration even in case of failures
	defer func(t time.Time) { duration.Set(time.Since(t).Seconds()) }(time.Now())

	l := l.WithFields(l.Fields{
		"Target":  q.Target,
		"Command": cmdLine,
	})

	conn, err := net.Dial("tcp", q.Target)
	if err != nil {
		l.Errorf("connection to %s failed: %s", q.Target, err)
		q.err = err
		return
	}

	res, err := nrpe.Run(conn, cmd, false, *timeout)
	if err != nil {
		q.err = err
		return
	}
	if res.StatusCode != 0 {
		l.WithField("statusCode", res.StatusCode).
			WithField("statusLine", res.StatusLine).
			Warnln("NRPE Response")
	} else {
		l.WithField("statusCode", res.StatusCode).
			WithField("statusLine", res.StatusLine).
			Debugln("NRPE Response")
	}

	status.Set(float64(res.StatusCode))
	outputs := strings.Split(res.StatusLine, "|")
	if len(outputs) == 2 {
		perfDataString := outputs[1]
		l = l.WithField("perfDataString", perfDataString)
		l.Debugln("Performance data string found.")
		for _, perf := range strings.Split(perfDataString, ", ") {
			a := strings.Split(strings.TrimSpace(perf), "=")
			if len(a) != 2 {
				l.Errorf("Error while parsing %s", perf)
				continue
			}
			key := a[0]
			value, err := strconv.ParseFloat(a[1], 64)

			if err != nil {
				l.Errorln("Error while converting", err)
				continue
			}
			metric, err := perfDatas.GetMetricWithLabelValues(key)
			if err != nil {
				l.Errorf("Error while setting label values", err)
				continue
			}
			metric.Set(value)

		}
	}
	q.err = nil
	return
}

// Describe implemented with dummy data to satisfy interface
func (q *Query) Describe(ch chan<- *prometheus.Desc) {
	ch <- prometheus.NewDesc("NRPE", "NRPE daemon metrics", nil, nil)
}

func handler(w http.ResponseWriter, r *http.Request) {
	params := r.URL.Query()
	l.Debugln("Params:", params)

	target := params.Get("target")
	if target == "" {
		http.Error(w, "Target parameter is missing", 400)
		return
	}
	port := params.Get("port")
	if port == "" {
		port = DEFAULT_NRPE_PORT // NRPE Default Port
	}
	target = fmt.Sprintf("%s:%s", target, port)
	cmd := params.Get("command")
	if cmd == "" {
		http.Error(w, "command parameter is missing", 400)
		return
	}
	lock := params.Get("lock") != "" && params.Get("lock") != "0"
	args := params["arg"]
	query, err := GetQuery(target, cmd, args, lock)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error while getting the query: %s", err), 500)
		return
	}
	registry := query.Registry

	query.o.Do(func() {
		query.wg.Add(1)
		query.Run()
		query.wg.Done()
	})

	query.wg.Wait()

	if query.err != nil {
		http.Error(w, fmt.Sprintf("Error while running the query: %s", query.err), 500 )
		return
	}
	h := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	h.ServeHTTP(w, r)
}

func main() {
	flag.Parse()

	if *verbose {
		l.SetLevel(l.DebugLevel)
	}
	listen := fmt.Sprintf("%s:%d", *listenAddr, *port)
	l.WithFields(l.Fields{"name": "nrpe-exporter", "version": version.Info(), "build": version.BuildContext(), "listen-addr": listen}).Info("Starting")
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>			
            <head>
            <title>NRPE Exporter</title>
            </head>
            <body>
            <h1>NRPE Exporter</h1>
				<p><a href="/metrics">This binary self-exported metrics</a></p>

				<h2>Format</h2>
				<p> "http://127.0.0.1:9235/run?target=|remote-nrpe-daemon-ip|&command=|nrpe-command|&Args=foo&arg=bar&arg=foobar&lock=1" </p>

				<p> You can use the lock=1 query parameter to prevent automagically concurrent executions. All queries will get the locked run results

            </body>
            </html>`))
	})

	http.HandleFunc("/run", func(w http.ResponseWriter, r *http.Request) {
		handler(w, r)
	})
	http.Handle("/metrics", promhttp.Handler())
	if err := http.ListenAndServe(listen, nil); err != nil {
		l.WithField("error", err).Errorf("Error while binding")
		os.Exit(1)
	}
}
