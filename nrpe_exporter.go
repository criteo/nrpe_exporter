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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/version"
	l "github.com/sirupsen/logrus"
	"github.com/criteo/nrpe_exporter/nrpe"
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

	defLabels      = []string{"command"}
	perfDataLabels = append(defLabels, "perf_key")
)

// PerfData is a basic key-value representation for Nagios's Perfdata
type PerfData struct {
	key   string
	value float64
}

// Command represent a Nagios's command (I.E: check_foo!bar!foobar)
type Command struct {
	command string
	args    []string
}

// NewCommand returns a new Command object
func NewCommand(command string, args []string) Command {
	return Command{command: command, args: args}
}

// Query is an instance of a command being run
type Query struct {
	sync.RWMutex
	command      Command
	metricsChans []chan prometheus.Metric
	target       string
	lock         bool
	hash         uint64
	exit         chan int
}

// NewQuery returns a new Query object
func NewQuery(target string, command string, args []string, lock bool) Query {

	return Query{
		command:      NewCommand(command, args),
		metricsChans: []chan prometheus.Metric{},
		target:       target,
		lock:         lock,
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

// CommandResult type describing the result of command against nrpe-server
type CommandResult struct {
	commandDuration float64
	statusOk        float64
	result          *nrpe.CommandResult
}

// Run Runs the query against the daemon TODO: Fix SSL
func (q *Query) Run() error {
	if q.lock {
		defer runningQueries.Delete(q.hash)
	}
	conn, err := net.Dial("tcp", q.target)
	if err != nil {
		return err
	}
	cmd := nrpe.NewCommand(q.command.command, q.command.args...)
	cmdLine := cmd.ToStatusLine()
	l := l.WithFields(l.Fields{
		"target":  q.target,
		"command": cmdLine,
	})
	start := time.Now()
	res, err := nrpe.Run(conn, cmd, false, *timeout)
	if err != nil {
		return err
	}
	l.WithField("statusCode", res.StatusCode).
		WithField("statusLine", res.StatusLine).
		Debugln("NRPE Response")
	duration := time.Since(start).Seconds()
	PerfDatas := []PerfData{}
	outputs := strings.Split(res.StatusLine, "|")
	if len(outputs) == 2 {
		perfDataString := outputs[1]
		l = l.WithField("perfDataString", perfDataString)
		l.Debugln("Performance data string found.")
		for _, perf := range strings.Split(perfDataString, ", ") {
			a := strings.Split(strings.TrimSpace(perf), "=")
			if len(a) != 2 {
				l.Warnf("Error while parsing %s", perf)
			}
			key := a[0]
			value, err := strconv.ParseFloat(a[1], 64)

			if err != nil {
				l.Warnln("Error while converting", err)
			}
			PerfDatas = append(PerfDatas, PerfData{key: key, value: value})

		}
	}
	//Check multiple status code passed in the URL
	for _, ch := range q.metricsChans {
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(prometheus.BuildFQName(NAMESPACE, "command", "duration"),
				"The time it took NRPE to execute the command",
				defLabels, nil),
			prometheus.GaugeValue,
			duration, cmdLine,
		)
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(prometheus.BuildFQName(NAMESPACE, "command", "status"), "The exit code of the NRPE command.", defLabels, nil),
			prometheus.GaugeValue,
			float64(res.StatusCode), cmdLine,
		)
		for _, p := range PerfDatas {
			ch <- prometheus.MustNewConstMetric(
				prometheus.NewDesc(prometheus.BuildFQName(NAMESPACE, "command", "perf_datas"), fmt.Sprintf("Performance data value"), perfDataLabels, nil),
				prometheus.GaugeValue,
				p.value, cmdLine, p.key,
			)
		}
		close(ch)
	}
	return nil
}

// Describe implemented with dummy data to satisfy interface
func (q *Query) Describe(ch chan<- *prometheus.Desc) {
	ch <- prometheus.NewDesc("NRPE", "NRPE daemon metrics", nil, nil)
}

// Collect Check for the state of the command and wait for the original query.
func (q *Query) Collect(ch chan<- prometheus.Metric) {
	sender := make(chan prometheus.Metric)
	q.Lock()
	q.metricsChans = append(q.metricsChans, sender)
	if len(q.metricsChans) == 1 {
		q.Unlock()
		go func() {
			err := q.Run()
			if err != nil {
				for _, ch := range q.metricsChans {
					close(ch)
				}
				l.Errorf("Error while running the command:", err)
			}
		}()

	} else {
		q.Unlock()
	}
	for m := range sender {
		ch <- m
	}
}

func handler(w http.ResponseWriter, r *http.Request) {
	params := r.URL.Query()
	l.Debugln("Params:", params)

	target := params.Get("target")
	if target == "" {
		http.Error(w, "Target parameter is missing", 400)
	}
	port := params.Get("port")
	if port == "" {
		port = DEFAULT_NRPE_PORT // NRPE Default Port
	}
	target = fmt.Sprintf("%s:%s", target, port)
	cmd := params.Get("command")
	if cmd == "" {
		http.Error(w, "Command parameter is missing", 400)
		return
	}
	lock := params.Get("lock") != "" && params.Get("lock") != "0"
	args := params["arg"]
	query, err := GetQuery(target, cmd, args, lock)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error while getting the query: %s", err), 500)
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(query)
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
				<p> "http://127.0.0.1:9235/run?target=|remote-nrpe-daemon-ip|&command=|nrpe-command|&args=foo&arg=bar&arg=foobar&lock=1" </p>

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
