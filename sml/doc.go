// Package sml provides functions for parsing and generating SML (SECS Message Language)
// representations of SECS-II messages, primarily used in the context of HSMS (High-Speed SECS Message Services).
//
// SML is a human-readable text-based format for representing SECS-II messages, which are used for
// communication between host systems and semiconductor manufacturing equipment.
//
// It supports various SECS-II data item types and provides options for handling different
// ASCII parsing modes:
//   - Strict Mode:  Adheres to the ASCII standard (character codes 32 to 126) and handles escape
//     characters (e.g., \n, \t) as literal characters. This is useful when parsing SML generated with
//     strict mode for SECS-II ASCII items (e.g., using secs2.WithStrictMode(true)).
//   - Non-Strict Mode (default): Allows non-printable ASCII characters and interprets escape characters
//     according to their usual meanings.
//
// Usage Example:
//
//	// Use strict mode
//	sml.WithStrictMode(true)
//	// Parse an SML string
//	messages, err := sml.ParseHSMS(`
//	    MessageName: 'S1F1' W
//	    <L[1]
//	        <A[4] 'test'>
//	    >
//	    .
//	`)
//	if err != nil {
//	    // Handle error
//	}
//	// Process the parsed HSMS data messages
package sml
