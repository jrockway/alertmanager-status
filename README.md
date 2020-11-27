# alertmanager-status

![CI](https://ci.jrock.us/api/v1/teams/main/pipelines/alertmanager-status/jobs/ci/badge)
[![codecov](https://codecov.io/gh/jrockway/alertmanager-status/branch/master/graph/badge.svg)](https://codecov.io/gh/jrockway/alertmanager-status)
[![](https://images.microbadger.com/badges/version/jrockway/alertmanager-status.svg)](https://microbadger.com/images/jrockway/alertmanager-status)

## Quick start

`alertmanger-status` is an
[Alertmanager webhook](https://prometheus.io/docs/alerting/latest/configuration/#webhook_config)
that serves a status page indicating whether or not it has received an alert recently. This allows
you to use a normal website monitoring service to alert you if Alertmanager stops publishing alerts.

To do this, you'll need an alert that is always firing. Many people have this already, but here's
one that I found laying around on the Internet:

```yaml
- name: Watchdog
  rules:
      - alert: AlwaysFiring
        expr: vector(1)
        for: 1s
        labels:
            severity: none
        annotations:
            summary: "AlwaysFiring"
            description: |
                This is an alert meant to ensure that the entire alerting pipeline is functional.
                This alert is always firing, therefore it should always be firing in Alertmanager.
```

Next, you'll need to [install](#Installation) `alertmanager-status` and tell Alertmanager to send
just this alert to its webhook endpoint. Your `alertmanager.yml` file should look something like
mine:

```yaml
global:
    resolve_timeout: 5m
route:
    group_by: ["job"]
    group_wait: 30s
    group_interval: 5m
    repeat_interval: 4h
    routes:
        - match:
              alertname: AlwaysFiring
          group_wait: 0s
          repeat_interval: 5s
          group_interval: 5s
          receiver: "status"
        - receiver: "discord"
    receiver: "null"
receivers:
    - name: "null"
    - name: "status"
      webhook_configs:
          - url: "http://alertmanager-status.monitoring.svc.cluster.local.:8081/webhook"
            send_resolved: false
    - name: "discord"
      webhook_configs:
          - url: "http://alertmanager-discord.monitoring.svc.cluster.local.:8080/"
```

What's going on here is that we setup a `status` webhook, which is the location of your
`alertmanager-status` instance's `/webhook` endpoint (it's served from the "debug" server), and set
up a route to match the `AlwaysFiring` alert. In that route, we send it to `status` instead of
`discord`, which is where I normally receive alerts. We also set a ridiculous repeat interval, so
that `alertmanager-status` can be aggressive about marking the alerting system unhealthy (the
default interval is 1 minute; no sign of that alert for 1 minute and we declare the system
unhealthy). Now when you visit the public part of `alertmanager-status`, you'll see either
`"alertmanager ok"` if it's sending that alert, or `"alertmanager unhealthy"` if it hasn't sent the
alert for one minute. From there, you can expose that status page to the Internet and tell your
favorite website monitoring service (I use [Oh Dear!](https://ohdear.app/)) to alert you when it
"goes down". Then you'll know if your Alertmanager setup stops sending alerts!

## Installation

If you're using Kubernetes, I have prepared a manifest. It uses `kustomize`, so you can just write
your site-specific configuration in a `kustomization.yaml` file that looks like:

```yaml
namespace: monitoring
bases:
    - github.com/jrockway/alertmanager-status/deploy?ref=v0.0.7
```

and `kubectl apply -k .` to the directory you put that file in. The release names on Github are tags
that you can use in the `?ref=...` directive.

You will need to add your own Ingress configuration if you want one. Create the manifest and add it
to your `kustomization.yaml` by adding a `resources` section that refers to it.

If you're not using Kubernetes, it's just a go program that takes configuration from command-line
flags or the environment and reads HTTP requests from the Internet. You can run `--help` for
details.

## Operation

`alertmanager-status` logs JSON-formatted structured logs at level=debug by default. The debug logs
are very verbose. The provided manifests change the level to info, which only contain logs that are
relevant to operators, and so are less verbose. State changes of the monitored alertmanager are
logged, and any time that "unhealthy" is served to your site-check service, a log message is
generated. That should serve to be informative and managable in terms of volume.

`alertmanager-status` binds two ports by default; "public" (8080) and "debug" (8081). "public"
serves the status page, and is intended to be exposed to the Internet for an external service to
probe. "debug" serves a readiness check (`/healthz`), a liveness check (`/livez`), the alertmanager
webhook (`/webhook`), a page of metrics for Prometheus to scrape (`/metrics`), and the usual
assortment of `net/http/pprof` endpoints.

A trace of every HTTP request is sent to Jaeger, if available. `alertmanager-status` is a Go app,
and you can configure Jager to your exact needs by following their
[documented environment variables](https://www.jaegertracing.io/docs/1.19/client-features/).

Metrics are made available to Prometheus via a page on the debug server, `/metrics`. We export a
variety of standard metrics, and some app-specific ones:

-   `alertmanager_status_alertmanager_health` - 1 if we consider Alertmanager healthy, 0 otherwise.

-   `alertmanager_status_alertmanager_last_healthy` - When Alertmanager was last confirmed to be
    healthy, in seconds since the Unix epoch.

-   `alertmanager_status_last_health_checked` - When your the health endpoint was last polled by
    your external health check service, in seconds since the Unix epoch. This lets you set up an
    alert to detect that your health checking service is down. Hopefully that doesn't happen at the
    same time your Alertmanager goes down!
