package helps

import "github.com/klauspost/compress/zstd"

// EncodeCodexOAuthBody encodes the final normalized JSON body for the
// ChatGPT OAuth HTTP wire profile.
func EncodeCodexOAuthBody(body []byte) ([]byte, error) {
	encoder, errNewEncoder := zstd.NewWriter(nil)
	if errNewEncoder != nil {
		return nil, errNewEncoder
	}
	defer encoder.Close()
	return encoder.EncodeAll(body, nil), nil
}
