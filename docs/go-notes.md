# Go notes for a C++ programmer

A running log, written as each idiom first shows up in this codebase. Each note says what the Go
idiom is, how it differs from the C++ habit, and where it is used here.

## Modules and packages

A *module* (`go.mod`) is the unit of versioning, roughly a CMake project plus its dependency
lockfile (`go.sum` pins exact hashes). A *package* is one directory; every file in the directory
shares one namespace, and there are no headers. Visibility is decided by the first letter of a
name: `Money` is exported (public), `amount` is package-private. There is no `protected` and no
`friend`. Code under `internal/` can only be imported by code rooted at the parent of `internal/`,
which the compiler enforces. This repo puts all domain logic in `internal/` so nothing outside
Ledgerline can depend on it.
