package client

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/snapshots"
	"github.com/docker/distribution/manifest/schema2"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/util/testutil/httpserver"
	"github.com/moby/buildkit/util/testutil/integration"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestClientIntegration(t *testing.T) {
	integration.Run(t, []integration.Test{
		testCallDiskUsage,
		testBuildMultiMount,
		testBuildHTTPSource,
		testBuildPushAndValidate,
		testResolveAndHosts,
		testUser,
		testOCIExporter,
		testWhiteoutParentDir,
		testDuplicateWhiteouts,
		testSchema1Image,
		testMountWithNoSource,
	})
}

func testCallDiskUsage(t *testing.T, sb integration.Sandbox) {
	t.Parallel()
	c, err := New(sb.Address())
	require.NoError(t, err)
	defer c.Close()
	_, err = c.DiskUsage(context.TODO())
	require.NoError(t, err)
}

func testBuildMultiMount(t *testing.T, sb integration.Sandbox) {
	t.Parallel()
	requiresLinux(t)
	c, err := New(sb.Address())
	require.NoError(t, err)
	defer c.Close()

	alpine := llb.Image("docker.io/library/alpine:latest")
	ls := alpine.Run(llb.Shlex("/bin/ls -l"))
	busybox := llb.Image("docker.io/library/busybox:latest")
	cp := ls.Run(llb.Shlex("/bin/cp -a /busybox/etc/passwd baz"))
	cp.AddMount("/busybox", busybox)

	def, err := cp.Marshal()
	require.NoError(t, err)

	err = c.Solve(context.TODO(), def, SolveOpt{}, nil)
	require.NoError(t, err)

	checkAllReleasable(t, c, sb, true)
}

func testBuildHTTPSource(t *testing.T, sb integration.Sandbox) {
	t.Parallel()

	c, err := New(sb.Address())
	require.NoError(t, err)
	defer c.Close()

	modTime := time.Now().Add(-24 * time.Hour) // avoid falso positive with current time

	resp := httpserver.Response{
		Etag:         identity.NewID(),
		Content:      []byte("content1"),
		LastModified: &modTime,
	}

	server := httpserver.NewTestServer(map[string]httpserver.Response{
		"/foo": resp,
	})
	defer server.Close()

	// invalid URL first
	st := llb.HTTP(server.URL + "/bar")

	def, err := st.Marshal()
	require.NoError(t, err)

	err = c.Solve(context.TODO(), def, SolveOpt{}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid response status 404")

	// first correct request
	st = llb.HTTP(server.URL + "/foo")

	def, err = st.Marshal()
	require.NoError(t, err)

	err = c.Solve(context.TODO(), def, SolveOpt{}, nil)
	require.NoError(t, err)

	require.Equal(t, server.Stats("/foo").AllRequests, 1)
	require.Equal(t, server.Stats("/foo").CachedRequests, 0)

	tmpdir, err := ioutil.TempDir("", "buildkit")
	require.NoError(t, err)
	defer os.RemoveAll(tmpdir)

	err = c.Solve(context.TODO(), def, SolveOpt{
		Exporter: ExporterLocal,
		ExporterAttrs: map[string]string{
			exporterLocalOutputDir: tmpdir,
		},
	}, nil)
	require.NoError(t, err)

	require.Equal(t, server.Stats("/foo").AllRequests, 2)
	require.Equal(t, server.Stats("/foo").CachedRequests, 1)

	dt, err := ioutil.ReadFile(filepath.Join(tmpdir, "foo"))
	require.NoError(t, err)
	require.Equal(t, []byte("content1"), dt)

	// test extra options
	st = llb.HTTP(server.URL+"/foo", llb.Filename("bar"), llb.Chmod(0741), llb.Chown(1000, 1000))

	def, err = st.Marshal()
	require.NoError(t, err)

	err = c.Solve(context.TODO(), def, SolveOpt{
		Exporter: ExporterLocal,
		ExporterAttrs: map[string]string{
			exporterLocalOutputDir: tmpdir,
		},
	}, nil)
	require.NoError(t, err)

	require.Equal(t, server.Stats("/foo").AllRequests, 3)
	require.Equal(t, server.Stats("/foo").CachedRequests, 1)

	dt, err = ioutil.ReadFile(filepath.Join(tmpdir, "bar"))
	require.NoError(t, err)
	require.Equal(t, []byte("content1"), dt)

	fi, err := os.Stat(filepath.Join(tmpdir, "bar"))
	require.NoError(t, err)
	require.Equal(t, fi.ModTime().Format(http.TimeFormat), modTime.Format(http.TimeFormat))
	require.Equal(t, int(fi.Mode()&0777), 0741)

	checkAllReleasable(t, c, sb, true)

	// TODO: check that second request was marked as cached
}

func testResolveAndHosts(t *testing.T, sb integration.Sandbox) {
	requiresLinux(t)
	t.Parallel()
	c, err := New(sb.Address())
	require.NoError(t, err)
	defer c.Close()

	busybox := llb.Image("busybox:latest")
	st := llb.Scratch()

	run := func(cmd string) {
		st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)
	}

	run(`sh -c "cp /etc/resolv.conf ."`)
	run(`sh -c "cp /etc/hosts ."`)

	def, err := st.Marshal()
	require.NoError(t, err)

	destDir, err := ioutil.TempDir("", "buildkit")
	require.NoError(t, err)
	defer os.RemoveAll(destDir)

	err = c.Solve(context.TODO(), def, SolveOpt{
		Exporter: ExporterLocal,
		ExporterAttrs: map[string]string{
			"output": destDir,
		},
	}, nil)
	require.NoError(t, err)

	dt, err := ioutil.ReadFile(filepath.Join(destDir, "resolv.conf"))
	require.NoError(t, err)
	require.Contains(t, string(dt), "nameserver")

	dt, err = ioutil.ReadFile(filepath.Join(destDir, "hosts"))
	require.NoError(t, err)
	require.Contains(t, string(dt), "127.0.0.1	localhost")

}

func testUser(t *testing.T, sb integration.Sandbox) {
	requiresLinux(t)
	t.Parallel()
	c, err := New(sb.Address())
	require.NoError(t, err)
	defer c.Close()

	st := llb.Image("busybox:latest").Run(llb.Shlex(`sh -c "mkdir -m 0777 /wd"`))

	run := func(user, cmd string) {
		st = st.Run(llb.Shlex(cmd), llb.Dir("/wd"), llb.User(user))
	}

	run("daemon", `sh -c "id -nu > user"`)
	run("daemon:daemon", `sh -c "id -ng > group"`)
	run("daemon:nogroup", `sh -c "id -ng > nogroup"`)
	run("1:1", `sh -c "id -g > userone"`)

	st = st.Run(llb.Shlex("cp -a /wd/. /out/"))
	out := st.AddMount("/out", llb.Scratch())

	def, err := out.Marshal()
	require.NoError(t, err)

	destDir, err := ioutil.TempDir("", "buildkit")
	require.NoError(t, err)
	defer os.RemoveAll(destDir)

	err = c.Solve(context.TODO(), def, SolveOpt{
		Exporter: ExporterLocal,
		ExporterAttrs: map[string]string{
			"output": destDir,
		},
	}, nil)
	require.NoError(t, err)

	dt, err := ioutil.ReadFile(filepath.Join(destDir, "user"))
	require.NoError(t, err)
	require.Contains(t, string(dt), "daemon")

	dt, err = ioutil.ReadFile(filepath.Join(destDir, "group"))
	require.NoError(t, err)
	require.Contains(t, string(dt), "daemon")

	dt, err = ioutil.ReadFile(filepath.Join(destDir, "nogroup"))
	require.NoError(t, err)
	require.Contains(t, string(dt), "nogroup")

	dt, err = ioutil.ReadFile(filepath.Join(destDir, "userone"))
	require.NoError(t, err)
	require.Contains(t, string(dt), "1")

	checkAllReleasable(t, c, sb, true)
}

func testOCIExporter(t *testing.T, sb integration.Sandbox) {
	requiresLinux(t)
	t.Parallel()
	c, err := New(sb.Address())
	require.NoError(t, err)
	defer c.Close()

	busybox := llb.Image("busybox:latest")
	st := llb.Scratch()

	run := func(cmd string) {
		st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)
	}

	run(`sh -c "echo -n first > foo"`)
	run(`sh -c "echo -n second > bar"`)

	def, err := st.Marshal()
	require.NoError(t, err)

	for _, exp := range []string{ExporterOCI, ExporterDocker} {

		destDir, err := ioutil.TempDir("", "buildkit")
		require.NoError(t, err)
		defer os.RemoveAll(destDir)

		out := filepath.Join(destDir, "out.tar")
		target := "example.com/buildkit/testoci:latest"

		err = c.Solve(context.TODO(), def, SolveOpt{
			Exporter: exp,
			ExporterAttrs: map[string]string{
				"output": out,
				"name":   target,
			},
		}, nil)
		require.NoError(t, err)

		dt, err := ioutil.ReadFile(out)
		require.NoError(t, err)

		m, err := readTarToMap(dt, false)
		require.NoError(t, err)

		_, ok := m["oci-layout"]
		require.True(t, ok)

		var index ocispec.Index
		err = json.Unmarshal(m["index.json"].data, &index)
		require.NoError(t, err)
		require.Equal(t, 2, index.SchemaVersion)
		require.Equal(t, 1, len(index.Manifests))

		var mfst ocispec.Manifest
		err = json.Unmarshal(m["blobs/sha256/"+index.Manifests[0].Digest.Hex()].data, &mfst)
		require.NoError(t, err)
		require.Equal(t, 2, len(mfst.Layers))

		var ociimg ocispec.Image
		err = json.Unmarshal(m["blobs/sha256/"+mfst.Config.Digest.Hex()].data, &ociimg)
		require.NoError(t, err)
		require.Equal(t, "layers", ociimg.RootFS.Type)
		require.Equal(t, 2, len(ociimg.RootFS.DiffIDs))

		_, ok = m["blobs/sha256/"+mfst.Layers[0].Digest.Hex()]
		require.True(t, ok)
		_, ok = m["blobs/sha256/"+mfst.Layers[1].Digest.Hex()]
		require.True(t, ok)

		if exp != ExporterDocker {
			continue
		}

		var dockerMfst []struct {
			Config   string
			RepoTags []string
			Layers   []string
		}
		err = json.Unmarshal(m["manifest.json"].data, &dockerMfst)
		require.NoError(t, err)
		require.Equal(t, 1, len(dockerMfst))

		_, ok = m[dockerMfst[0].Config]
		require.True(t, ok)
		require.Equal(t, 2, len(dockerMfst[0].Layers))
		require.Equal(t, 1, len(dockerMfst[0].RepoTags))
		require.Equal(t, target, dockerMfst[0].RepoTags[0])

		for _, l := range dockerMfst[0].Layers {
			_, ok := m[l]
			require.True(t, ok)
		}
	}

	checkAllReleasable(t, c, sb, true)
}

func testBuildPushAndValidate(t *testing.T, sb integration.Sandbox) {
	requiresLinux(t)
	t.Parallel()
	c, err := New(sb.Address())
	require.NoError(t, err)
	defer c.Close()

	busybox := llb.Image("busybox:latest")
	st := llb.Scratch()

	run := func(cmd string) {
		st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)
	}

	run(`sh -c "mkdir -p foo/sub; echo -n first > foo/sub/bar; chmod 0741 foo;"`)
	run(`true`) // this doesn't create a layer
	run(`sh -c "echo -n second > foo/sub/baz"`)

	def, err := st.Marshal()
	require.NoError(t, err)

	registry, err := sb.NewRegistry()
	if errors.Cause(err) == integration.ErrorRequirements {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	target := registry + "/buildkit/testpush:latest"

	err = c.Solve(context.TODO(), def, SolveOpt{
		Exporter: ExporterImage,
		ExporterAttrs: map[string]string{
			"name": target,
			"push": "true",
		},
	}, nil)
	require.NoError(t, err)

	// test existence of the image with next build
	firstBuild := llb.Image(target)

	def, err = firstBuild.Marshal()
	require.NoError(t, err)

	destDir, err := ioutil.TempDir("", "buildkit")
	require.NoError(t, err)
	defer os.RemoveAll(destDir)

	err = c.Solve(context.TODO(), def, SolveOpt{
		Exporter: ExporterLocal,
		ExporterAttrs: map[string]string{
			"output": destDir,
		},
	}, nil)
	require.NoError(t, err)

	dt, err := ioutil.ReadFile(filepath.Join(destDir, "foo/sub/bar"))
	require.NoError(t, err)
	require.Equal(t, dt, []byte("first"))

	dt, err = ioutil.ReadFile(filepath.Join(destDir, "foo/sub/baz"))
	require.NoError(t, err)
	require.Equal(t, dt, []byte("second"))

	fi, err := os.Stat(filepath.Join(destDir, "foo"))
	require.NoError(t, err)
	require.Equal(t, 0741, int(fi.Mode()&0777))

	checkAllReleasable(t, c, sb, false)

	// examine contents of exported tars (requires containerd)
	var cdAddress string
	if cd, ok := sb.(interface {
		ContainerdAddress() string
	}); !ok {
		return
	} else {
		cdAddress = cd.ContainerdAddress()
	}

	// TODO: make public pull helper function so this can be checked for standalone as well

	client, err := containerd.New(cdAddress)
	require.NoError(t, err)
	defer client.Close()

	ctx := namespaces.WithNamespace(context.Background(), "buildkit")

	// check image in containerd
	_, err = client.ImageService().Get(ctx, target)
	require.NoError(t, err)

	// deleting image should release all content
	err = client.ImageService().Delete(ctx, target, images.SynchronousDelete())
	require.NoError(t, err)

	checkAllReleasable(t, c, sb, true)

	img, err := client.Pull(ctx, target)
	require.NoError(t, err)

	desc, err := img.Config(ctx)
	require.NoError(t, err)

	dt, err = content.ReadBlob(ctx, img.ContentStore(), desc.Digest)
	require.NoError(t, err)

	var ociimg ocispec.Image
	err = json.Unmarshal(dt, &ociimg)
	require.NoError(t, err)

	require.NotEqual(t, "", ociimg.OS)
	require.NotEqual(t, "", ociimg.Architecture)
	require.NotEqual(t, "", ociimg.Config.WorkingDir)
	require.Equal(t, "layers", ociimg.RootFS.Type)
	require.Equal(t, 2, len(ociimg.RootFS.DiffIDs))
	require.NotNil(t, ociimg.Created)
	require.True(t, time.Since(*ociimg.Created) < 2*time.Minute)
	require.Condition(t, func() bool {
		for _, env := range ociimg.Config.Env {
			if strings.HasPrefix(env, "PATH=") {
				return true
			}
		}
		return false
	})

	require.Equal(t, 3, len(ociimg.History))
	require.Contains(t, ociimg.History[0].CreatedBy, "foo/sub/bar")
	require.Contains(t, ociimg.History[1].CreatedBy, "true")
	require.Contains(t, ociimg.History[2].CreatedBy, "foo/sub/baz")
	require.False(t, ociimg.History[0].EmptyLayer)
	require.True(t, ociimg.History[1].EmptyLayer)
	require.False(t, ociimg.History[2].EmptyLayer)

	dt, err = content.ReadBlob(ctx, img.ContentStore(), img.Target().Digest)
	require.NoError(t, err)

	var mfst schema2.Manifest
	err = json.Unmarshal(dt, &mfst)
	require.NoError(t, err)

	require.Equal(t, schema2.MediaTypeManifest, mfst.MediaType)
	require.Equal(t, 2, len(mfst.Layers))

	dt, err = content.ReadBlob(ctx, img.ContentStore(), mfst.Layers[0].Digest)
	require.NoError(t, err)

	m, err := readTarToMap(dt, true)
	require.NoError(t, err)

	item, ok := m["foo/"]
	require.True(t, ok)
	require.Equal(t, int32(item.header.Typeflag), tar.TypeDir)
	require.Equal(t, 0741, int(item.header.Mode&0777))

	item, ok = m["foo/sub/"]
	require.True(t, ok)
	require.Equal(t, int32(item.header.Typeflag), tar.TypeDir)

	item, ok = m["foo/sub/bar"]
	require.True(t, ok)
	require.Equal(t, int32(item.header.Typeflag), tar.TypeReg)
	require.Equal(t, []byte("first"), item.data)

	_, ok = m["foo/sub/baz"]
	require.False(t, ok)

	dt, err = content.ReadBlob(ctx, img.ContentStore(), mfst.Layers[1].Digest)
	require.NoError(t, err)

	m, err = readTarToMap(dt, true)
	require.NoError(t, err)

	item, ok = m["foo/sub/baz"]
	require.True(t, ok)
	require.Equal(t, int32(item.header.Typeflag), tar.TypeReg)
	require.Equal(t, []byte("second"), item.data)

	item, ok = m["foo/"]
	require.True(t, ok)
	require.Equal(t, int32(item.header.Typeflag), tar.TypeDir)
	require.Equal(t, 0741, int(item.header.Mode&0777))

	item, ok = m["foo/sub/"]
	require.True(t, ok)
	require.Equal(t, int32(item.header.Typeflag), tar.TypeDir)

	_, ok = m["foo/sub/bar"]
	require.False(t, ok)
}

// containerd/containerd#2119
func testDuplicateWhiteouts(t *testing.T, sb integration.Sandbox) {
	requiresLinux(t)
	t.Parallel()
	c, err := New(sb.Address())
	require.NoError(t, err)
	defer c.Close()

	busybox := llb.Image("busybox:latest")
	st := llb.Scratch()

	run := func(cmd string) {
		st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)
	}

	run(`sh -c "mkdir -p d0 d1; echo -n first > d1/bar;"`)
	run(`sh -c "rm -rf d0 d1"`)

	def, err := st.Marshal()
	require.NoError(t, err)

	destDir, err := ioutil.TempDir("", "buildkit")
	require.NoError(t, err)
	defer os.RemoveAll(destDir)

	out := filepath.Join(destDir, "out.tar")

	err = c.Solve(context.TODO(), def, SolveOpt{
		Exporter: ExporterOCI,
		ExporterAttrs: map[string]string{
			"output": out,
		},
	}, nil)
	require.NoError(t, err)

	dt, err := ioutil.ReadFile(out)
	require.NoError(t, err)

	m, err := readTarToMap(dt, false)
	require.NoError(t, err)

	var index ocispec.Index
	err = json.Unmarshal(m["index.json"].data, &index)
	require.NoError(t, err)

	var mfst ocispec.Manifest
	err = json.Unmarshal(m["blobs/sha256/"+index.Manifests[0].Digest.Hex()].data, &mfst)
	require.NoError(t, err)

	lastLayer := mfst.Layers[len(mfst.Layers)-1]

	layer, ok := m["blobs/sha256/"+lastLayer.Digest.Hex()]
	require.True(t, ok)

	m, err = readTarToMap(layer.data, true)
	require.NoError(t, err)

	_, ok = m[".wh.d0"]
	require.True(t, ok)

	_, ok = m[".wh.d1"]
	require.True(t, ok)

	// check for a bug that added whiteout for subfile
	_, ok = m["d1/.wh.bar"]
	require.True(t, !ok)
}

// #276
func testWhiteoutParentDir(t *testing.T, sb integration.Sandbox) {
	requiresLinux(t)
	t.Parallel()
	c, err := New(sb.Address())
	require.NoError(t, err)
	defer c.Close()

	busybox := llb.Image("busybox:latest")
	st := llb.Scratch()

	run := func(cmd string) {
		st = busybox.Run(llb.Shlex(cmd), llb.Dir("/wd")).AddMount("/wd", st)
	}

	run(`sh -c "mkdir -p foo; echo -n first > foo/bar;"`)
	run(`rm foo/bar`)

	def, err := st.Marshal()
	require.NoError(t, err)

	destDir, err := ioutil.TempDir("", "buildkit")
	require.NoError(t, err)
	defer os.RemoveAll(destDir)

	out := filepath.Join(destDir, "out.tar")

	err = c.Solve(context.TODO(), def, SolveOpt{
		Exporter: ExporterOCI,
		ExporterAttrs: map[string]string{
			"output": out,
		},
	}, nil)
	require.NoError(t, err)

	dt, err := ioutil.ReadFile(out)
	require.NoError(t, err)

	m, err := readTarToMap(dt, false)
	require.NoError(t, err)

	var index ocispec.Index
	err = json.Unmarshal(m["index.json"].data, &index)
	require.NoError(t, err)

	var mfst ocispec.Manifest
	err = json.Unmarshal(m["blobs/sha256/"+index.Manifests[0].Digest.Hex()].data, &mfst)
	require.NoError(t, err)

	lastLayer := mfst.Layers[len(mfst.Layers)-1]

	layer, ok := m["blobs/sha256/"+lastLayer.Digest.Hex()]
	require.True(t, ok)

	m, err = readTarToMap(layer.data, true)
	require.NoError(t, err)

	_, ok = m["foo/.wh.bar"]
	require.True(t, ok)

	_, ok = m["foo/"]
	require.True(t, ok)
}

// #296
func testSchema1Image(t *testing.T, sb integration.Sandbox) {
	t.Parallel()
	c, err := New(sb.Address())
	require.NoError(t, err)
	defer c.Close()

	st := llb.Image("gcr.io/google_containers/pause:3.0@sha256:0d093c962a6c2dd8bb8727b661e2b5f13e9df884af9945b4cc7088d9350cd3ee")

	def, err := st.Marshal()
	require.NoError(t, err)

	err = c.Solve(context.TODO(), def, SolveOpt{}, nil)
	require.NoError(t, err)

	checkAllReleasable(t, c, sb, true)
}

// #319
func testMountWithNoSource(t *testing.T, sb integration.Sandbox) {
	t.Parallel()
	c, err := New(sb.Address())
	require.NoError(t, err)
	defer c.Close()

	busybox := llb.Image("docker.io/library/busybox:latest")
	st := llb.Scratch()

	var nilState llb.State

	// This should never actually be run, but we want to succeed
	// if it was, because we expect an error below, or a daemon
	// panic if the issue has regressed.
	run := busybox.Run(
		llb.Args([]string{"/bin/true"}),
		llb.AddMount("/nil", nilState, llb.SourcePath("/"), llb.Readonly))

	st = run.AddMount("/mnt", st)

	def, err := st.Marshal()
	require.NoError(t, err)

	err = c.Solve(context.TODO(), def, SolveOpt{}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "has no input")

	checkAllReleasable(t, c, sb, true)
}

func requiresLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("unsupported GOOS: %s", runtime.GOOS)
	}
}

type tarItem struct {
	header *tar.Header
	data   []byte
}

func readTarToMap(dt []byte, compressed bool) (map[string]*tarItem, error) {
	m := map[string]*tarItem{}
	var r io.Reader = bytes.NewBuffer(dt)
	if compressed {
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		r = gz
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err != nil {
			if err == io.EOF {
				return m, nil
			}
			return nil, err
		}
		if _, ok := m[h.Name]; ok {
			return nil, errors.Errorf("duplicate entries for %s", h.Name)
		}

		var dt []byte
		if h.Typeflag == tar.TypeReg {
			dt, err = ioutil.ReadAll(tr)
			if err != nil {
				return nil, err
			}
		}
		m[h.Name] = &tarItem{header: h, data: dt}
	}
}

func checkAllReleasable(t *testing.T, c *Client, sb integration.Sandbox, checkContent bool) {
	retries := 0
loop0:
	for {
		require.True(t, 20 > retries)
		retries++
		du, err := c.DiskUsage(context.TODO())
		require.NoError(t, err)
		for _, d := range du {
			if d.InUse {
				time.Sleep(500 * time.Millisecond)
				continue loop0
			}
		}
		break
	}

	err := c.Prune(context.TODO(), nil)
	require.NoError(t, err)

	du, err := c.DiskUsage(context.TODO())
	require.NoError(t, err)
	require.Equal(t, 0, len(du))

	// examine contents of exported tars (requires containerd)
	var cdAddress string
	if cd, ok := sb.(interface {
		ContainerdAddress() string
	}); !ok {
		return
	} else {
		cdAddress = cd.ContainerdAddress()
	}

	// TODO: make public pull helper function so this can be checked for standalone as well

	client, err := containerd.New(cdAddress)
	require.NoError(t, err)
	defer client.Close()

	ctx := namespaces.WithNamespace(context.Background(), "buildkit")
	snapshotService := client.SnapshotService("overlayfs")

	retries = 0
	for {
		count := 0
		err = snapshotService.Walk(ctx, func(context.Context, snapshots.Info) error {
			count++
			return nil
		})
		require.NoError(t, err)
		if count == 0 {
			break
		}
		require.True(t, 20 > retries)
		retries++
		time.Sleep(500 * time.Millisecond)
	}

	if !checkContent {
		return
	}

	retries = 0
	for {
		count := 0
		err = client.ContentStore().Walk(ctx, func(content.Info) error {
			count++
			return nil
		})
		require.NoError(t, err)
		if count == 0 {
			break
		}
		require.True(t, 20 > retries)
		retries++
		time.Sleep(500 * time.Millisecond)
	}
}
