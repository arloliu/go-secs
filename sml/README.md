# SML Parser

The `sml` package provides an `HSMSParser` to parse SML (SECS Message Language) representations of SECS-II messages, commonly used with HSMS (High-Speed SECS Message Services).

Since SML lacks a single, definitive standard, various alternative definitions exist. These variations include different quoting styles for stream and function codes, optional message names, line comments, and different quoting for ASCII data items.

This package strives to support these different formats through configuration options.

## Supported SML Features

### Stream and Function Code Quoting

* `hsms.UseStreamFunctionSingleQuote()` (default): Use single quotes around `'SxFy'`.
* `hsms.UseStreamFunctionDoubleQuote()`: Use double quotes around `"SxFy"`.
* `hsms.UseStreamFunctionNoQuote()`: No quotes around `SxFy`.

### ASCII Data Item Quoting

* `secs2.UseASCIIDoubleQuote()` (default): Use double quotes around the value, e.g., `<A "ascii value">`.
* `secs2.UseASCIISingleQuote()`: Use single quotes around the value, e.g., `<A 'ascii value'>`.

### ASCII Parsing Modes

* **Non-Strict Mode (default):**
    * Optimizes for performance by assuming the ASCII string does not contain the same quote character as the enclosing quotes.
    * Does not handle escape sequences.
    * May lead to inaccurate parsing if the same quote character is present in the data. Use strict mode for more reliable parsing in such cases.
    * Set with `sml.WithStrictMode(false)`.

* **Strict Mode:**
    * Adheres to the ASCII standard (character codes 32 to 126).
    * Supports parsing non-printable ASCII characters represented by their decimal values (e.g., `0x0A` for newline).
    * Set with `sml.WithStrictMode(true)`.

#### Example with Strict Mode

```text
// sml.WithStrictMode(true)

// Handles non-printable ASCII newline (0x0A)
MessageName: 'S99F100' W <A 'first line' 0x0A 'second line'>.

// secs2.UseASCIISingleQuote()
// Parses this ASCII data item value to: a 'single quote' string
'S99F100' W <A 'a \'single-quote\' string'>.

// secs2.UseASCIIDoubleQuote()
// Parses this ASCII data item value to: a 'single quote' and "double-quote" string
'S99F100' W <A "a 'single-quote' and \"double-quote\" string">.
```

### Multi-line ASCII Data Item Values
Both strict and non-strict modes support multi-line ASCII values, but they handle newlines differently.

For example, let's assume it has a multi-line string:
```
one
two
three
```

Strict Mode: Represents newlines as 0x0A.
```
<A 'one' 0x0A 'two' 0x0A 'three'>
```

Non-Strict Mode:  Preserves newlines as is.
```
<A 'one
two
three'
>
```
