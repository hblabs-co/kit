package mongo

// Environment variable keys read by this package. They used to live
// in the shared constants package as EnvMongoURL / EnvMongoDatabase;
// the keys belong to the package that reads them.
const (
	// EnvURL is the mongodb:// (or mongodb+srv:// for SRV records)
	// connection string. Includes scheme, credentials, host(s), and
	// replica-set / TLS params. Example:
	// "mongodb+srv://user:pass@cluster.example.net/app?retryWrites=true&w=majority".
	EnvURL = "HB_MONGO_URL"

	// EnvDatabase is the database name the Mongo client targets.
	// Empty → the client uses whatever database is encoded in
	// EnvURL's path; if both are unset, a "hblabs" default kicks
	// in (so local dev works without explicit config).
	EnvDatabase = "HB_MONGO_DATABASE"
)
