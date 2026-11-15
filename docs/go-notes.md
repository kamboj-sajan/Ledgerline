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
