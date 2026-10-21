package session

import (
	"strings"

	"github.com/google/uuid"
)

const psfOwnerStopPrefix = "owner_stop:"

func psfOwnerStopRequestID() string {
	return psfOwnerStopPrefix + uuid.NewString()
}

func psfIsOwnerStopRequestID(requestID string) bool {
	return strings.HasPrefix(requestID, psfOwnerStopPrefix)
}

func psfOwnerStopSQLPredicate(alias string) string {
	column := "request_id"
	if alias != "" {
		column = alias + ".request_id"
	}
	return column + " GLOB '" + psfOwnerStopPrefix + "*'"
}
