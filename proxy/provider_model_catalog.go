package proxy

import (
	"errors"
	"fmt"
	"io"
)

const (
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
