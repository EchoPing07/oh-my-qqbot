package tools

import "context"

// contextT is the package-local alias for context.Context (the tools never
// need the concrete type beyond signatures).
type contextT = context.Context
