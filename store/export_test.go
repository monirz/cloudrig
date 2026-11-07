package store

// SetImportBatch makes an import commit every n entries, and returns a func
// that restores the default.
func SetImportBatch(n int) func() {
	old := importBatch
	importBatch = n
	return func() { importBatch = old }
}

// InterruptImportAfter makes an import stop once n entries are in, as a
// process killed mid-import would, and returns a func that undoes it.
func InterruptImportAfter(n int) func() {
	importInterrupted = func(imported int) bool { return imported >= n }
	return func() { importInterrupted = nil }
}
