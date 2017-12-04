# NRPE Exporter for Prometheus

## Disclaimer: This project is inspired from the RobustPerception's [exporter](https://github.com/RobustPerception/nrpe_exporter) This is not an official Criteo product

## Building & Running
    go get -v github.com/criteo/nrpe_exporter
    cd $GOPATH/src/github.com/criteo/nrpe_exporter
    dep ensure -vendor-only
    go install

## What to expect ?

1. This project aims to be a complete drop-in replacement for a Nagios based monitoring
2. At the moment only numerics (float) performance-datas are exported, we plan to support even metric-system ones (ie: size=40GB) in a near future



## Example Prometheus scrape-config
```
scrape_configs:
  
   - job_name: 'nrpe'
     scrape_interval: "60s"
     metrics_path: /run
     params:
         arg: ['10','5']
 #        port: ['5666'] #Default 
         command: ['foo']
     static_configs:
       - targets:
           - server.example.com
     relabel_configs:
       - source_labels: ['__address__']
         target_label: '__param_target'
       - source_labels: ['__param_target']
         target_label: 'instance'
       - target_label: '__address__'
         replacement: "localhost:9235"
```
