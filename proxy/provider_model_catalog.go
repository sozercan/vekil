package proxy

import (
	"errors"
	"fmt"
	"io"
)

const (
	// maxProviderModelCatalogBodySize caps successful decoded /models payloads
	// at 4 MiB. Real provider catalogs are metadata and normally far smaller;
	// this still allows thousands of entries while bounding allocation after the
	// HTTP transport has decompressed gzip or other supported encodings.
	maxProviderModelCatalogBodySize = 4 << 20
)

var errProviderModelCatalogTooLarge = errors.New("provider model catalog exceeds size limit")

func readProviderModelCatalogBody(reader io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maxProviderModelCatalogBodySize+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxProviderModelCatalogBodySize {
		return nil, fmt.Errorf("%w: limit %d bytes", errProviderModelCatalogTooLarge, maxProviderModelCatalogBodySize)
	}
	return body, nil
}
