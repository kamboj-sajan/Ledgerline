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

## Named results and errors from `defer`

`func runMigrate(...) (err error)` gives the return value a name. A deferred closure can then
read and change it after the `return` statement has set it:
`defer func() { err = errors.Join(err, m.Close()) }()`. That way an error from `Close` is
reported without overwriting an earlier one. A plain `defer m.Close()` would silently throw the
`Close` error away. In C++, a throwing destructor cannot report anything safely, so this has no
direct equivalent there.

## Enums are typed constants

Go has no `enum class`. The idiom is a named type plus constants of that type:
`type AccountCode string` and `const MerchantPayable AccountCode = "merchant_payable"`. A plain
`string` cannot be passed where an `AccountCode` is expected without an explicit conversion,
which gives some of `enum class`'s type safety. Unlike C++, any string can still be converted
into an `AccountCode`, so validity is checked at runtime (`AccountKey.validate`), and the SQL
`CHECK` constraint backs it up. Named types can have methods: `MerchantPayable.Type()`. Used in:
`internal/ledger/ledger.go`.

## Slices alias their backing array

A slice is a small header (pointer, length, capacity) that points at a shared array, roughly
`std::span` plus a capacity. Copying a struct that contains a slice copies the header, not the
elements. So `Memory.Post` stores `slices.Clone(t.Postings)`: otherwise a caller who modified its
own slice after posting would silently rewrite ledger history. `TestMemoryStoresACopy` checks
this.

## Structs as map keys

Any comparable type can be a map key, including structs of comparable fields. So
`map[AccountKey]money.Money` works with no hash function to write, unlike
`std::unordered_map`, which needs a `std::hash` specialisation. Map iteration order is
*deliberately randomised*, so code that needs determinism sorts the keys first (see
`Memory.Accounts`).

## Goroutines, `sync.WaitGroup` and closing a channel as a starting gun

`go f()` starts `f` on a *goroutine*, a thread managed by the Go runtime and multiplexed onto a
few OS threads. Goroutines are cheap enough (a few KB of stack) to start hundreds per test.
`wg.Go(func(){...})` (Go 1.25+) starts a goroutine and tracks it, and `wg.Wait()` blocks until
all of them return: a `std::latch` plus `std::jthread` in one. In
`TestLedgerEnsureAccountConcurrent`, every goroutine first blocks on `<-start`, a receive from a
channel. `close(start)` wakes *all* receivers at once, so the 50 workers hit Postgres together
instead of trickling in. The loop variable `i` is captured per iteration (Go 1.22+ gives each
iteration a fresh variable, which fixed the classic closure-captures-the-loop-variable bug).

## Generics

`func auditIDs[T any](...) (int64, []T, error)` is a generic function, like a function template
with a constraint. `any` means no constraint. Generic code is type-checked once at definition,
not per instantiation as C++ templates are, so there are no SFINAE-style surprises. The
trade-off is that you can only use operations the constraint allows. Used in
`internal/ledger/audit.go` to scan both text and bigint ID columns with one helper.

## Interfaces at the consumer

`ledger.DB` is a three-method interface declared in the package that *uses* it. `*pgxpool.Pool`,
`*pgx.Conn` and `pgx.Tx` all satisfy it without knowing it exists, so `ledger.Balance` runs
inside or outside a transaction. The Go habit is to declare small interfaces where they are
consumed, not big ones where types are defined. A C++ base class has to be planned up front;
a Go interface can be declared after the fact around types you don't own. `storetest.TB` does
the same, so `*rapid.T` can be passed where tests normally pass `*testing.T`.

## Custom error types: `errors.As` and `Unwrap`

A sentinel (`ErrInvalidTransition`) says *what kind* of failure happened. When the caller also
needs details, return a struct that implements `error`: `*payments.TransitionError` carries the
event and the from/to states. Its `Unwrap() error` method returns `ErrInvalidTransition`, so
`errors.Is(err, payments.ErrInvalidTransition)` still matches, and
`errors.As(err, &te)` gets the struct back (like `dynamic_cast` on a caught exception). The API
uses `errors.Is` to pick the HTTP status and the struct's `Error()` text for the message.

## `net/http` routing and handlers

Since Go 1.22 the standard `http.ServeMux` matches method and path patterns:
`mux.Handle("POST /v1/payments/{id}/capture", h)`, and the handler reads `r.PathValue("id")`. No
router dependency is needed. When two patterns match, the more specific one wins, and a pattern
with a method is more specific than the same path without one. Ledgerline uses that rule to
register a method-less fallback per path that answers a JSON 405. A handler is anything with
`ServeHTTP(w, r)`. `http.HandlerFunc(f)` turns a plain function into one; `acquirer.Func` uses
the same trick for the acquirer interface. Middleware is a function that takes a handler and
returns a wrapping handler (`s.authenticate(h)`, `s.recoverPanics(...)`).

## `httptest` and `context.WithoutCancel`

`httptest.NewServer(handler)` runs a real HTTP server on a random local port for tests. The API
tests go through the full stack, real sockets included. `context.WithoutCancel(ctx)` keeps a
context's values but drops its cancellation. Ledgerline uses it to release an idempotency key
even when the client has already disconnected (which cancels `r.Context()`), because that
cleanup must still happen.

## Build tags for fault injection

`internal/failpoint` has two files. `failpoint.go` (`//go:build !chaos`) defines an empty
`Inject`, which the compiler inlines away, so a normal binary contains no fault-injection code
at all. `failpoint_chaos.go` (`//go:build chaos`) defines the real one, which kills the process
with SIGKILL. This is Go's equivalent of `#ifdef CHAOS` around a whole translation unit.

## `os/exec`, signals, and why SIGKILL matters

`exec.CommandContext(ctx, name, args...)` builds a child process; `cmd.Start()` runs it and
`cmd.Wait()` reaps it, like `fork`/`exec`/`waitpid`. The chaos test supervises children with a
goroutine per process that calls `Wait` and restarts the process when it exits.
`signal.NotifyContext` turns SIGINT/SIGTERM into context cancellation for graceful shutdown.
SIGKILL cannot be caught or handled at all: no `defer` runs, no transaction is rolled back by
the client, and no response is sent. That is exactly the crash the chaos test needs, and why
`failpoint.Inject` sends SIGKILL to its own process rather than calling `os.Exit` or panicking.

## `crypto/hmac` and constant-time comparison

`hmac.New(sha256.New, key)` gives a `hash.Hash`: `Write` the message, then call `Sum`.
`hmac.Equal(a, b)` compares in time that does not depend on where the first difference is, so an
attacker cannot learn a valid signature byte by byte from response times, which a C++ `memcmp`
or `==` would allow.

## Function values and closures

Go functions are first-class values, like `std::function` but with no allocation or type
erasure to think about for most uses. `eval.Sweep(tune, test, candidates, firesAt)` takes
`firesAt func(threshold float64, e Example) bool`. Each rule passes a closure that captures its
own config and calls `fraud.Config.Fires`, so one sweep routine serves every rule.
`flag := func(e Example) bool { return firesAt(th, e) }` captures the loop variable `th`. Since
Go 1.22 that is safe, because each iteration gets its own copy.
