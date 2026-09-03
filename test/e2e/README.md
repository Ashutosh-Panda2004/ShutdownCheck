# End-to-end tests

These cover the command-line surface: what gets written where, in what format,
and with which exit code.

Verdict correctness across stacks is the [conformance suite](../conformance)'s
job. Keeping the two apart stops both from half-testing the same thing and
leaving a gap in the middle.

What is asserted here:

- a correct service passes end to end — the anchor for everything else
- `--format json` parses as the versioned public schema
- reports and badges are written with `0600`, since they carry internal
  hostnames and URLs
- a recorded run can be re-judged later under a different profile, which is the
  point of separating measurement from interpretation
- `--trials` aggregates rather than reporting only the last run
- an unreachable target exits distinctly from a real defect, so a pipeline can
  tell "your service is broken" from "the tool never got started"
- `--capture-target-logs` puts the target's own output on the timeline

The tests build the shared Go server from `../conformance/go` rather than
keeping a fixture of their own; a second copy would be one more thing to keep in
step.

Killing processes needs real signals, so these are Unix-only, and they skip
under `-short`.

```sh
go test ./test/e2e/
```
