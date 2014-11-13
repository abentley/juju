// Copyright 2014 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package backups_test

import (
	"bytes"
	"io"

	gc "gopkg.in/check.v1"

	"github.com/juju/juju/api/backups"
)

type uploadSuite struct {
	baseSuite
}

var _ = gc.Suite(&uploadSuite{})

func (s *uploadSuite) TestUploadFake(c *gc.C) {
	var sshHost, sshFilename string
	s.PatchValue(backups.TestSSHUpload, func(host, filename string, archive io.Reader) error {
		sshHost = host
		sshFilename = filename
		return nil
	})

	original := []byte("<compressed>")
	archive := bytes.NewBuffer(original)
	id, err := s.client.Upload(archive)
	c.Assert(err, gc.IsNil)

	c.Check(sshHost, gc.Equals, "ubuntu@127.0.0.1")
	c.Check(sshFilename, gc.Matches, `juju-backup-.*\.tgz$`)
	c.Check(id, gc.Matches, `file://juju-backup-.*\.tgz$`)
}
