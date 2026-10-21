// Package httpsrc streams bounded HTTPS sources (RASL, FISL, URL import) through
// SSRF and redirect guards. It does not verify content: callers hash the bytes
// and report verification failures.
package httpsrc
