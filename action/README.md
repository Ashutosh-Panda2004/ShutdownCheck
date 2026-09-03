# GitHub Action

The published Action wrapper, giving one-line CI adoption:

```yaml
- uses: shutdowncheck/shutdowncheck-action@v1
  with:
    config: shutdowncheck.yaml
    scenario: api
    comment-on-pr: true
```

It runs the check, uploads the JSON report as an artifact, writes the Markdown
report to `$GITHUB_STEP_SUMMARY`, and optionally posts or updates a pull request
comment.

This is the primary distribution channel for the project, which is why it was
pulled forward from v1.3 in the original draft into the v1.0 scope.

Built in Phase 8.
