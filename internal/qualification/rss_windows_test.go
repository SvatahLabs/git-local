//go:build windows

package qualification

// Native Windows memory qualification requires a Job Object sampler. Zero
// marks RSS as unavailable rather than reporting an invented measurement.
func peakRSSBytes() (int64, int64, error) { return 0, 0, nil }
