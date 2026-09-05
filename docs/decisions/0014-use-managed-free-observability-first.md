# 14. Use managed free observability first

**Status:** accepted

## Context

The two-person team needs metrics, logs, traces and alerts, but the small EC2 host
should spend its memory on the API and workers rather than an observability
database.

## Decision

Send Prometheus and OpenTelemetry signals through Grafana Alloy to Grafana Cloud
Free. Use its 14-day raw retention and free-tier limits. Keep small daily SLO
summary rows for 90 days and alert at 80 percent of any ingestion or active-series
limit.

## Consequences

The service gets one managed place for dashboards and alerts without another
stateful service on EC2. A telemetry outage does not stop product work, and local
buffers must remain bounded.

Longer raw retention or paid ingestion requires a separate cost decision. The
OpenTelemetry and Prometheus protocols keep that provider change replaceable.
