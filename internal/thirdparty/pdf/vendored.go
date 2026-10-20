package pdf

// This directory vendors github.com/ledongthuc/pdf at
// v0.0.0-20260907135840-6c8c28e0e8a0 (BSD-3-Clause, see LICENSE). The
// sources are copied verbatim (name.go re-gofmt'ed only); upstream tests,
// examples and the pdfpasswd command are not included.
//
// The plugin runs inside the CLIProxyAPI process and feeds this package
// PDFs that arrive in client requests, so every local change on top of the
// upstream copy is marked "opencode-go hardening" and exists to keep a
// corrupt or hostile file from hanging or exhausting the host process.
// Diff against the upstream version above before pulling in new upstream
// code, and keep those changes.
