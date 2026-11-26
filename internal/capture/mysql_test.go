package capture

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"yc-agent/internal/capture/mysql"
	"yc-agent/internal/config"
)

func TestEveryMySQLArtifactHasADataType(t *testing.T) {
	metadata := mysql.NewMetadata(mysql.Target{}, "test", time.Now(), "")

	seen := map[string]bool{}

	for _, collector := range mysql.Collectors(metadata, "all", time.Second, time.Minute, 5*time.Minute) {
		artifact := collector.Artifact()

		dt := mySampledDataType(artifact)
		assert.NotEmpty(t, dt, "%s would be written and never uploaded", artifact.Name)
		assert.False(t, seen[dt], "%s shares a dt", artifact.Name)
		assert.Equal(t, artifact.Name+".txt", artifact.FileName)

		seen[dt] = true
	}

	assert.Len(t, seen, 16, "the sixteen files of spec §7")
}

func TestMySQLFrequencyFloorIsTheStatementTimeout(t *testing.T) {
	assert.Equal(t, mysql.StatementTimeout, config.MinFrequency,
		"config floors frequency at the capture's per-statement timeout; the packages do not "+
			"import each other, so the spellings are pinned equal here")
}

func TestMySQLTargetRedactsThroughEveryVerb(t *testing.T) {
	task := &MySQLCapture{Target: &config.MySQL{DBTarget: config.DBTarget{Host: "db", Password: "s3cret"}}}

	for _, rendered := range []string{fmt.Sprintf("%v", task), fmt.Sprintf("%+v", task), fmt.Sprintf("%#v", task)} {
		assert.NotContains(t, rendered, "s3cret")
	}

	target := mysqlTarget(&config.MySQL{DBTarget: config.DBTarget{
		Host: "db", Port: 3306, TLS: &config.TargetTLS{VerifyServerCertificate: new(bool)},
	}})
	assert.False(t, target.TLS.Disabled)
	assert.Equal(t, "skip-verify", target.TLS.Mode())
}

func TestAMariaDBTargetIsAWarning(t *testing.T) {
	logs := captureLogOutput(t)

	warnMariaDB(mysql.Metadata{MySQLVariant: mysql.VariantMySQL, Version: "8.4.3"})
	assert.Empty(t, logs.String())

	warnMariaDB(mysql.Metadata{MySQLVariant: mysql.VariantMariaDB, Version: "10.11.6-MariaDB"})
	assert.Contains(t, logs.String(), `"level":"warn"`)
	assert.Contains(t, logs.String(), "MariaDB 10.11.6-MariaDB, which the MySQL capture does not support")
}
