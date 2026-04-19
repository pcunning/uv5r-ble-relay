package bleconn

import "time"

// WriteChunked splits data into <=chunkSize segments and feeds each to write.
// An optional pace is slept between segments. It returns the first error
// encountered. The signature uses a function value so tests can record calls
// without involving a real BLE characteristic.
func WriteChunked(write func([]byte) (int, error), data []byte, chunkSize int, pace time.Duration) error {
	if chunkSize <= 0 {
		chunkSize = 20
	}
	for i := 0; i < len(data); {
		end := i + chunkSize
		if end > len(data) {
			end = len(data)
		}
		if _, err := write(data[i:end]); err != nil {
			return err
		}
		i = end
		if pace > 0 && i < len(data) {
			time.Sleep(pace)
		}
	}
	return nil
}
