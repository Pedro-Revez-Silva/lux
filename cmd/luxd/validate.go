package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/marcioapm/lux/internal/blob"
)

// servePlan is what serve has settled from its configuration before it
// opens any connection: nothing in it has touched the network.
type servePlan struct {
	db *pgxpool.Config
	s3 blob.Config
}

// prepareServe runs every check serve makes on its configuration before
// connecting to PostgreSQL or S3. validate runs exactly this, so a release
// whose validate passes is refused by serve only for reasons outside the
// configuration (a database or bucket it cannot reach, AWS host settings).
func prepareServe(c config) (servePlan, error) {
	dsn, err := require(c.Database.URL, "database.url", "LUX_DATABASE_URL")
	if err != nil {
		return servePlan{}, err
	}
	bucket, err := require(c.S3.Bucket, "s3.bucket", "LUX_S3_BUCKET")
	if err != nil {
		return servePlan{}, err
	}
	// pgx's parse error quotes the string with only best-effort password
	// redaction: the message names the key and nothing of its value.
	db, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return servePlan{}, errors.New("database.url (LUX_DATABASE_URL) is not a valid PostgreSQL connection string")
	}
	// The S3 client is built by serve once the database is open: loading the
	// AWS configuration may call IMDS or resolve a credentials host. Only
	// lux's own S3 settings are checked here.
	if err := checkS3(c); err != nil {
		return servePlan{}, err
	}
	// http.Server binds only after the database and bucket answer: a
	// malformed address is refused here instead. Empty is net/http's ":http".
	if c.Listen != "" {
		if err := checkListen(c.Listen); err != nil {
			return servePlan{}, fmt.Errorf("listen (LUX_LISTEN) %q: %w", c.Listen, err)
		}
	}
	return servePlan{db: db, s3: blob.Config{
		Endpoint:       c.S3.Endpoint,
		PublicEndpoint: c.S3.PublicEndpoint,
		Region:         c.S3.Region,
		Bucket:         bucket,
		AccessKey:      c.S3.AccessKey,
		SecretKey:      c.S3.SecretKey,
	}}, nil
}

// checkListen accepts host:port where port is empty (net.Listen then picks
// one, as it always has) or a decimal 0-65535. The host is not resolved.
func checkListen(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("want host:port")
	}
	if port == "" {
		return nil
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return errors.New("want a port number 0-65535, not a service name")
	}
	return nil
}

func checkS3(c config) error {
	for _, e := range []struct{ v, key, env string }{
		{c.S3.Endpoint, "s3.endpoint", "LUX_S3_ENDPOINT"},
		{c.S3.PublicEndpoint, "s3.public_endpoint", "LUX_S3_PUBLIC_ENDPOINT"},
	} {
		if e.v == "" {
			continue
		}
		// The value is not quoted: an endpoint URL may carry userinfo.
		if u, err := url.Parse(e.v); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%s (%s): want an absolute http or https URL", e.key, e.env)
		}
	}
	// An empty variable is ignored, so only the file can clear the default.
	if c.S3.Region == "" {
		return errors.New("s3.region (LUX_S3_REGION) is empty: set a region or remove the key (default us-east-1)")
	}
	// blob.New pairs access_key with whatever secret_key is, and ignores a
	// lone secret_key in favour of the AWS default chain.
	if (c.S3.AccessKey == "") != (c.S3.SecretKey == "") {
		return errors.New("s3.access_key and s3.secret_key (LUX_S3_ACCESS_KEY, LUX_S3_SECRET_KEY): set both or neither")
	}
	return nil
}

// loadServe is the configuration half of serve: load the file and the
// environment, then prepareServe. serve and validate both start here.
func loadServe(path string) (config, string, servePlan, error) {
	c, file, err := loadConfigFile(path)
	if err != nil {
		return c, file, servePlan{}, err
	}
	plan, err := prepareServe(c)
	return c, file, plan, err
}

// validate refuses what serve would refuse on configuration grounds, and
// connects to nothing. On success it prints the file it read.
func validate(path string, stdout io.Writer) error {
	_, file, _, err := loadServe(path)
	if err != nil {
		return err
	}
	if file == "" {
		file = "no file"
	}
	_, err = fmt.Fprintln(stdout, "ok:", file)
	return err
}
