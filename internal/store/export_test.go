package store

// MigrateTo lets tests seed a database as an older luxd left it, then
// apply the migrations after it.
var MigrateTo = migrate

// Migration is a migration's SQL by version, for a test that applies one
// out of order, as a luxd without the ones before it did.
func Migration(version string) ([]byte, error) {
	return migrations.ReadFile("migrations/" + version + ".sql")
}
