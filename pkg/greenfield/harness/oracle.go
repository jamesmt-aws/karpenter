/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package harness

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/test"
)

// SchedulerOracle runs the real kube-scheduler as a subprocess against the
// envtest apiserver. envtest ships only apiserver+etcd -- no scheduler, no
// kubelet -- so pod binding never happens unless we run the scheduler
// ourselves. The oracle is the independent authority the POC compares
// karpenter's scheduling simulation against: fabricate a Node with
// Harness.MakeNodeFromShape, start the oracle, and ask WaitForBinding whether
// the real scheduler agrees the pods fit.
type SchedulerOracle struct {
	// BinaryPath is the kube-scheduler binary actually used.
	BinaryPath string
	// KubeconfigPath is the kubeconfig written for the envtest apiserver.
	KubeconfigPath string
	// LogPath receives the scheduler's combined stdout/stderr.
	LogPath string

	kubeClient client.Client
	cmd        *exec.Cmd
	done       chan struct{}
	exitErr    error
}

// StartSchedulerOracle locates or downloads a kube-scheduler binary matching
// the envtest apiserver's version, writes a kubeconfig for the envtest admin
// user (client certs with group system:masters, so RBAC is bypassed), and
// starts the scheduler with leader election disabled and its HTTPS serving
// port off. workDir holds the kubeconfig and log (use t.TempDir()); the
// binary itself is cached under ~/.local/share/kubebuilder-envtest so it is
// downloaded at most once per version.
func StartSchedulerOracle(ctx context.Context, env *test.Environment, workDir string) (*SchedulerOracle, error) {
	versionInfo, err := env.KubernetesInterface.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("discovering apiserver version: %w", err)
	}
	version := versionInfo.GitVersion // e.g. "v1.36.2"

	binaryPath, err := locateKubeScheduler(ctx, version)
	if err != nil {
		return nil, err
	}
	kubeconfigPath := filepath.Join(workDir, "scheduler-oracle.kubeconfig")
	if err := writeKubeconfig(env.Config, kubeconfigPath); err != nil {
		return nil, fmt.Errorf("writing kubeconfig: %w", err)
	}
	logPath := filepath.Join(workDir, "kube-scheduler.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}

	// --secure-port=0 disables the scheduler's own HTTPS endpoint entirely, so
	// no port conflicts between parallel suites and no authn/authz lookups of
	// the extension-apiserver-authentication ConfigMap.
	cmd := exec.Command(binaryPath,
		"--kubeconfig="+kubeconfigPath,
		"--leader-elect=false",
		"--secure-port=0",
		"--v=3",
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Kill the scheduler if the test process dies without running Stop.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("starting kube-scheduler: %w", err)
	}
	o := &SchedulerOracle{
		BinaryPath:     binaryPath,
		KubeconfigPath: kubeconfigPath,
		LogPath:        logPath,
		kubeClient:     env.Client,
		cmd:            cmd,
		done:           make(chan struct{}),
	}
	go func() {
		o.exitErr = cmd.Wait()
		_ = logFile.Close()
		close(o.done)
	}()
	// Fail fast if the scheduler dies immediately (bad flag, unreachable
	// apiserver) rather than timing out later in WaitForBinding.
	select {
	case <-o.done:
		return nil, fmt.Errorf("kube-scheduler exited at startup: %v\n--- log tail ---\n%s", o.exitErr, tailFile(logPath, 4096))
	case <-time.After(1500 * time.Millisecond):
	}
	return o, nil
}

// Stop terminates the scheduler subprocess: SIGTERM, then SIGKILL after 10s.
func (o *SchedulerOracle) Stop() error {
	if o.cmd == nil || o.cmd.Process == nil {
		return nil
	}
	select {
	case <-o.done:
		return nil // already exited
	default:
	}
	_ = o.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-o.done:
	case <-time.After(10 * time.Second):
		_ = o.cmd.Process.Kill()
		<-o.done
	}
	return nil
}

// WaitForBinding polls spec.nodeName on each pod until all are bound or the
// timeout elapses. It returns a map of "namespace/name" to the node each pod
// was bound to. If the scheduler process exits while waiting, or the timeout
// is hit, the error includes the tail of the scheduler log.
func (o *SchedulerOracle) WaitForBinding(ctx context.Context, pods []*corev1.Pod, timeout time.Duration) (map[string]string, error) {
	bound := map[string]string{}
	err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
		select {
		case <-o.done:
			return false, fmt.Errorf("kube-scheduler exited while waiting for bindings: %v", o.exitErr)
		default:
		}
		for _, pod := range pods {
			key := client.ObjectKeyFromObject(pod)
			if _, ok := bound[key.String()]; ok {
				continue
			}
			p := &corev1.Pod{}
			if err := o.kubeClient.Get(ctx, key, p); err != nil {
				return false, err
			}
			if p.Spec.NodeName != "" {
				bound[key.String()] = p.Spec.NodeName
			}
		}
		return len(bound) == len(pods), nil
	})
	if err != nil {
		var unbound []string
		for _, pod := range pods {
			key := client.ObjectKeyFromObject(pod).String()
			if _, ok := bound[key]; !ok {
				unbound = append(unbound, key)
			}
		}
		return bound, fmt.Errorf("waiting for pod bindings (unbound: %s): %w\n--- kube-scheduler log tail ---\n%s",
			strings.Join(unbound, ", "), err, tailFile(o.LogPath, 8192))
	}
	return bound, nil
}

// locateKubeScheduler finds a kube-scheduler binary matching version (e.g.
// "v1.36.2"). Search order: the kubebuilder-envtest cache dir (both the flat
// kube-scheduler-<version> name and the setup-envtest style
// k8s/<version>-<os>-<arch>/ layout), then PATH (version-checked), then
// download from dl.k8s.io into the cache.
func locateKubeScheduler(ctx context.Context, version string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	cacheDir := filepath.Join(home, ".local", "share", "kubebuilder-envtest")
	candidates := []string{
		filepath.Join(cacheDir, "kube-scheduler-"+version),
		filepath.Join(cacheDir, "k8s", fmt.Sprintf("%s-%s-%s", strings.TrimPrefix(version, "v"), runtime.GOOS, runtime.GOARCH), "kube-scheduler"),
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	if pathBin, err := exec.LookPath("kube-scheduler"); err == nil {
		if matchesVersion(pathBin, version) {
			return pathBin, nil
		}
	}
	dst := candidates[0]
	if err := downloadKubeScheduler(ctx, version, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// matchesVersion reports whether `bin --version` output contains version.
func matchesVersion(bin, version string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").CombinedOutput()
	return err == nil && strings.Contains(string(out), version)
}

// downloadKubeScheduler fetches the official release binary from
// https://dl.k8s.io/<version>/bin/<os>/<arch>/kube-scheduler into dst
// (write-to-temp, chmod +x, atomic rename).
func downloadKubeScheduler(ctx context.Context, version, dst string) error {
	url := fmt.Sprintf("https://dl.k8s.io/%s/bin/%s/%s/kube-scheduler", version, runtime.GOOS, runtime.GOARCH)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading %s: HTTP %d", url, resp.StatusCode)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".kube-scheduler-download-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// writeKubeconfig serializes a rest.Config (the envtest admin config) into a
// kubeconfig file that kube-scheduler's --kubeconfig flag can consume.
func writeKubeconfig(restConfig *rest.Config, path string) error {
	caData, err := dataOrFile(restConfig.CAData, restConfig.CAFile)
	if err != nil {
		return fmt.Errorf("reading CA data: %w", err)
	}
	certData, err := dataOrFile(restConfig.CertData, restConfig.CertFile)
	if err != nil {
		return fmt.Errorf("reading client cert: %w", err)
	}
	keyData, err := dataOrFile(restConfig.KeyData, restConfig.KeyFile)
	if err != nil {
		return fmt.Errorf("reading client key: %w", err)
	}
	host := restConfig.Host
	if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
		host = "https://" + host
	}
	const name = "envtest"
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters[name] = &clientcmdapi.Cluster{
		Server:                   host,
		CertificateAuthorityData: caData,
		InsecureSkipTLSVerify:    len(caData) == 0 && restConfig.Insecure,
	}
	cfg.AuthInfos[name] = &clientcmdapi.AuthInfo{
		ClientCertificateData: certData,
		ClientKeyData:         keyData,
		Token:                 restConfig.BearerToken,
		Username:              restConfig.Username,
		Password:              restConfig.Password,
	}
	cfg.Contexts[name] = &clientcmdapi.Context{Cluster: name, AuthInfo: name}
	cfg.CurrentContext = name
	return clientcmd.WriteToFile(*cfg, path)
}

func dataOrFile(data []byte, file string) ([]byte, error) {
	if len(data) > 0 {
		return data, nil
	}
	if file == "" {
		return nil, nil
	}
	return os.ReadFile(file)
}

// tailFile returns up to maxBytes from the end of path, best effort.
func tailFile(path string, maxBytes int64) string {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf("(unable to read %s: %v)", path, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return fmt.Sprintf("(unable to stat %s: %v)", path, err)
	}
	if info.Size() > maxBytes {
		if _, err := f.Seek(-maxBytes, io.SeekEnd); err != nil {
			return fmt.Sprintf("(unable to seek %s: %v)", path, err)
		}
	}
	out, err := io.ReadAll(f)
	if err != nil {
		return fmt.Sprintf("(unable to read %s: %v)", path, err)
	}
	return string(out)
}
