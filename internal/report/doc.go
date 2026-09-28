// Package report renders an analysis result: the human terminal report with the
// shutdown timeline visualisation, plus the JSON, JUnit XML, Markdown, NDJSON,
// HTML and SVG badge formats.
//
// Renderers are pure with respect to their input report; they take the output
// writer as a parameter so every format is covered by golden-file tests.
//
// Implemented in Phase 5.
package report
