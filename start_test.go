package daemon_test

import (
	"testing"

	"github.com/malivvan/daemon"
	"github.com/stretchr/testify/assert"
)

func TestIsEphemeralExecutableDetectsTestBinaries(t *testing.T) {
	assert := assert.New(t)

	assert.True(daemon.IsEphemeralExecutable("/tmp/tool.test"))
	assert.True(daemon.IsEphemeralExecutable(`C:\Temp\tool.test.exe`))
	assert.True(daemon.IsEphemeralExecutable(`C:\Temp\tool.TEST.EXE`))
	assert.False(daemon.IsEphemeralExecutable("/usr/local/bin/tool"))
}

func TestIsEphemeralExecutableDetectsGoBuildPaths(t *testing.T) {
	assert.True(t, daemon.IsEphemeralExecutable("/tmp/go-build123/tool"))
	assert.True(t, daemon.IsEphemeralExecutable(`C:\Users\me\AppData\Local\Temp\go-build123\tool.exe`))
}
