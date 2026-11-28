package cli

// noReadyPolicy is a ready policy that requires no field, for a listing test
// whose subject is not the field-presence gate.
func noReadyPolicy() ([]string, error) { return nil, nil }
