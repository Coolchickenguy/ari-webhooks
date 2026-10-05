package ingest

// CurrentEvidenceVersion identifies the evidence algorithm shipped by this
// build. Bump it whenever a change requires every stored submission to be
// recaptured by the reingest worker.
const CurrentEvidenceVersion = 2
