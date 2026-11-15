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

## Errors are values, not exceptions

Go has no exceptions for ordinary failures. A function that can fail returns an extra `error`
result, and the caller checks it right there: `sum, err := a.Add(b); if err != nil { ... }`.
Sentinel errors are package-level values (`var ErrOverflow = errors.New(...)`), similar to an
error-code enum but with a message attached. You add context with `fmt.Errorf("...: %w", err)`;
`%w` wraps the error and keeps it inspectable, and `errors.Is(err, money.ErrOverflow)` walks the
wrap chain the way `catch (const OverflowError&)` would match a derived type. `panic` exists, but
it is reserved for programmer bugs, like a failed `assert`. `money.New` panics on the zero
`Currency` because that is a bug, while `ParseCurrency` returns an error because bad input is
expected. Used in: `internal/money`.

## Multiple return values

`func (m Money) Add(o Money) (Money, error)` returns two values. This replaces C++ out-parameters
and `std::expected`. The `value, ok := m[key]` and `value, err := f()` shapes are everywhere.

## Signed overflow is defined behaviour (and silent)

In C++ signed overflow is UB. In Go it is defined: it wraps around in two's complement, silently.
So `a + b` on `int64` never traps and never optimises away; it just gives the wrong number.
That's why `Money.Add` checks `a > math.MaxInt64-b` *before* adding. For fee maths,
`math/bits.Mul64` gives the full 128-bit product (like `__int128`), so `amount × bps` cannot
overflow before the division. Used in: `internal/money/money.go`.

## Zero values and "constructors"

Every type has a zero value (`0`, `""`, `nil`, or a struct with all fields zeroed). Go has no
constructors, so `New...` functions are just the convention. Making fields unexported
(`amount`, `currency`) means other packages *must* go through `money.New`/`ParseCurrency`. The one
thing they can still write is the zero value `money.Money{}`, which is why the code treats a zero
`Currency` as invalid.

## Value receivers and immutability

`func (m Money) Add(...)` has a *value receiver*: `m` is a copy, like a C++ method taking
`*this` by value. Go has no `const` methods, so an immutable type in Go is one that only has value
receivers and returns new values. `func (c *Currency) UnmarshalText(...)` has a *pointer receiver*
because it has to modify the caller's variable. Structs whose fields are all comparable support
`==` for free, so `got != money.New(5, money.USD)` compares amount and currency.

## Interfaces are satisfied implicitly

There is no `implements` and no inheritance. A type satisfies an interface just by having the
methods. `Money` has `String() string`, so it is a `fmt.Stringer` and `fmt.Println` uses it.
`Currency` has `MarshalText`/`UnmarshalText`, so `encoding/json` encodes it as a JSON string even
though `money` never imports anything about JSON interfaces by name. It works like C++20 concepts
checked at the point of use, except it also works at runtime through interface values.

## Table-driven tests, subtests, external test packages

Tests are ordinary functions `TestXxx(t *testing.T)` in `_test.go` files, run by `go test`. The
idiom is a slice of anonymous structs (one row per case) and a loop that calls `t.Run(name, ...)`,
so each row reports as its own subtest (`-run 'TestParseMajor/12.34_USD'` runs one). Files with
`package money_test` (note the suffix) are compiled as a *separate* package that can only use the
exported API. That keeps tests honest, like testing only through a public header. `Example`
functions with an `// Output:` comment are both documentation and a test: `go test` checks the
printed output.

## Property-based tests with rapid

`rapid.Check` runs a function on many generated inputs (100 by default, change it with
`-args -rapid.checks=N`). On failure it *shrinks* the input to a minimal counterexample and
prints it. The money properties compare against `math/big` as an oracle that cannot overflow.
A quick mutation check (removing the overflow guard in `Add` via `go test -overlay`) was caught
after 2 generated cases.

## `range` over an integer

`for i := range len(s)` counts `0..len(s)-1` (Go 1.22+), the same as `for (int i = 0; i < n; ++i)`.

## `context.Context`

Every function that does I/O or might block takes a `ctx context.Context` as its first parameter.
A context carries a cancellation signal and a deadline down the call stack. When it is cancelled
(Ctrl-C, client disconnect, timeout), pgx aborts the query and `http.Client` aborts the request.
C++ has no standard equivalent: the closest thing is passing a `std::stop_token` everywhere by
hand. Contexts are passed explicitly, never stored in structs. `signal.NotifyContext` in
`cmd/ledgerline/main.go` turns SIGINT/SIGTERM into cancellation. In tests, `t.Context()` is
cancelled when the test ends.

## `defer`

`defer m.Close()` schedules a call for when the *surrounding function* returns, however it
returns. It plays the role RAII destructors play in C++, but it is explicit and scoped to the
function, not to a block. Deferred calls run last-in-first-out. One trap: `os.Exit` ends the
process immediately and skips every deferred call. That is why `TestMain` calls a helper
(`runTests`) that holds the defers and returns an exit code, then calls `os.Exit` with that code.

## Blank imports for side effects

`import _ "github.com/jackc/pgx/v5/stdlib"` imports a package only for its `init()` function,
which registers the `"pgx"` driver with `database/sql`. This is like linking in a translation
unit whose static initialiser registers a plugin. It is used once, in `internal/store/migrate.go`,
because goose needs a `database/sql` handle. The application itself uses pgx's native pool.

## `//go:embed`

`//go:embed *.sql` above `var FS embed.FS` makes the compiler bake the matching files into the
binary, like `#embed` in C23/C++26 or a generated resource file. The `ledgerline` binary can
migrate a database without the `.sql` files on disk, and the Docker image ships only the binary.

## Build tags

A file that starts with `//go:build integration` is compiled only when you pass
`-tags=integration`, similar to an `#ifdef` around the whole file. `go test ./...` therefore
stays fast and needs no Docker, while `make test-integration` includes the Postgres tests.
Later, the `chaos` tag will compile failpoints into the binary only for fault-injection tests.

## `flag.FlagSet` for subcommands

The standard `flag` package parses `-name value` flags. A `flag.NewFlagSet` per subcommand
(`migrate`, later `serve`, `dispatch`) gives `git`-style CLIs without a dependency.

## Concurrency primitives in the test harness

`sync.Mutex` is `std::mutex`, but it is a value, not a handle (never copy one after first use;
`go vet` checks this). `atomic.Int64` is `std::atomic<int64_t>`. A zero-value `sync.Mutex` is
unlocked and ready to use, so it needs no constructor.
