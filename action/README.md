# ShutdownCheck GitHub Action

Runs the check in CI, writes the report to the job summary, uploads the JSON as
an artifact, and optionally keeps a single up-to-date comment on the pull
request.

It is a **composite** action rather than a container action: every step is
readable in [action.yml](action.yml), and using it adds no image to your supply
chain. It downloads the released binary and verifies its checksum before running
anything.

## Quick start

```yaml
- uses: shutdowncheck/shutdowncheck@v1
  with:
    url: http://localhost:8080/api/orders
    readiness-url: http://localhost:8080/readyz
    command: ./bin/my-server --port 8080
```

The step fails when a defect is found, so it works as a merge gate as it stands.

## Against a container

```yaml
- uses: shutdowncheck/shutdowncheck@v1
  with:
    docker: my-api
    url: /api/orders
    readiness-url: /readyz
```

Path-only URLs are completed from the container's published ports, so the
ephemeral host port does not have to be discovered first.

## Reporting without gating

Useful while you are finding out how bad things currently are:

```yaml
- uses: shutdowncheck/shutdowncheck@v1
  with:
    url: http://localhost:8080/health
    command: ./bin/my-server
    fail-on-error: 'false'
    comment-on-pr: 'true'
```

`comment-on-pr` needs `pull-requests: write`:

```yaml
permissions:
  contents: read
  pull-requests: write
```

## Inputs

| Input | Default | Description |
| --- | --- | --- |
| `version` | `latest` | Version of shutdowncheck to use, e.g. `v1.2.3` |
| `url` | | URL to send load to during the check |
| `readiness-url` | | Readiness endpoint, so SC007 and SC008 can be evaluated |
| `command` | | Command to start and terminate |
| `docker` | | Name or id of a running container to target instead |
| `config` | | Path to a `shutdowncheck.yaml` |
| `scenario` | | Scenario from the config file to run |
| `profile` | `kubernetes` | `auto`, `standalone`, `strict`, `lame-duck`, `kubernetes`, `docker` |
| `grace-period` | | Time allowed before escalating to `SIGKILL` |
| `trials` | `1` | Repeat count; the worst result across trials is the verdict |
| `args` | | Extra flags passed to `shutdowncheck run` verbatim |
| `fail-on-error` | `true` | Fail the step when a defect is found |
| `comment-on-pr` | `false` | Post or update a sticky pull request comment |
| `upload-artifact` | `true` | Upload the JSON report |
| `artifact-name` | `shutdowncheck-report` | Name of the uploaded artifact |

## Outputs

| Output | Description |
| --- | --- |
| `verdict` | `PASS`, `FAIL` or `INCONCLUSIVE` |
| `score` | Shutdown score from 0 to 100 |
| `exit-code` | The tool's exit code |
| `report-path` | Path to the JSON report on the runner |

Pin a version rather than tracking `latest` if you want a run to be reproducible
six months from now.

## How it decides

The run is recorded once as NDJSON and then rendered twice — once as JSON for
the artifact, once as Markdown for the summary. Your service is terminated
exactly once, and every artifact describes that same measurement.

`INCONCLUSIVE` is never treated as a pass. If the tool could not prove correct
behaviour, the step reports that rather than waving the build through.

Exit codes `3`, `4` and `5` mean the tool never ran the experiment — a bad
invocation, an unreachable target, or a bug in the tool. In those cases there is
no report, and the step fails with the error rather than inventing a verdict.

The `jq` paths this action depends on are pinned by a test in the Go code
(`TestActionJSONPathsAreStable`), so a change to the report schema breaks the
build here rather than silently breaking your pipeline.

## Requirements

`jq` and `curl`, both present on all GitHub-hosted runners. Process and command
targets need POSIX signals, so use a Linux or macOS runner, or target a
container.
