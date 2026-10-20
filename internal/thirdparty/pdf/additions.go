package pdf

// opencode-go additions: accessors the plugin needs that upstream does not
// export. They only read existing state.

// Ref returns the object number and generation of the indirect object v
// was loaded from, or of the nearest enclosing indirect object for a
// direct value (for example a font dictionary written inline in a
// /Resources dictionary). Values not read from a file report 0, 0.
// Callers combine it with the key they followed to identify a value.
func (v Value) Ref() (id uint32, gen uint16) {
	return v.ptr.id, v.ptr.gen
}
