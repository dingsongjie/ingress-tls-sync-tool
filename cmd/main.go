package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	log "github.com/dingsongjie/file-help/pkg/log"
	"github.com/go-co-op/gocron/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"www.github.com/dingsongjie/ingress-tls-2-local-nginx/pkg/cmd"
)

type SyncConfig struct {
	Certs                []CertConfig `json:"certs"`
	NginxReloadCmd       string       `json:"nginxReloadCmd"`
	NginxTestCmd         string       `json:"nginxTestCmd"`
	TriggerIntervalHours int          `json:"triggerIntervalHours"`
}
type CertConfig struct {
	SecretNameSapce string `json:"secretNamespace"`
	SecretName      string `json:"secretName"`
	CertPath        string `json:"certPath"`
}

var (
	certSyncToolStatus prometheus.Counter
	renewTimes         prometheus.Counter
	debugNginx         = true
)

func main() {
	var kubeconfig *string
	var syncConfigPath *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	var addr = flag.String("listen-address", "0.0.0.0:8080", "The address to listen on for HTTP requests.")
	syncConfigPath = flag.String("syncConfig", "", "absolute path to the syncConfig json file")

	flag.Parse()

	// create the clientset
	clientset := k8sClient(kubeconfig)
	var config = syncConfig(syncConfigPath)

	log.Initialise()
	logger := log.Logger
	defer logger.Sync()

	reg := prometheus.NewRegistry()
	certSyncToolStatus = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "dqy_cert_sync_status",
			Help: "the status of dangquyun tls sync to lumi",
		})
	renewTimes = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "dqy_cert_renew_times",
			Help: "the renew times of tls in lumi",
		})
	// Add go runtime metrics and process collectors.
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		certSyncToolStatus,
		renewTimes,
	)

	s, err := gocron.NewScheduler()
	if err != nil {
		log.Logger.Fatal("Create schedule faild")
	}
	_, err = s.NewJob(
		gocron.DurationJob(time.Duration(config.TriggerIntervalHours)*time.Hour),
		gocron.NewTask(
			sync,
			clientset,
			config,
		),
		gocron.JobOption(gocron.WithStartImmediately()),
	)
	if err != nil {
		log.Logger.Fatal("Create sync job faild")
	}
	s.Start()
	defer s.Shutdown()
	// Expose /metrics HTTP endpoint using the created custom registry.
	http.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	log.Logger.Fatal((http.ListenAndServe(*addr, nil)).Error())

	// sync(clientset, config)
	// ticker := time.Tick(time.Duration(config.TriggerIntervalHours) * time.Hour)
	// for range ticker {
	// 	sync(clientset, config)
	// }

}
func k8sClient(kubeConfig *string) *kubernetes.Clientset {

	config, err := clientcmd.BuildConfigFromFlags("", *kubeConfig)
	if err != nil {
		logError(err.Error())
	}

	// create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		logError(err.Error())
	}
	return clientset
}
func syncConfig(syncConfig *string) *SyncConfig {

	jsonData, err := ioutil.ReadFile(*syncConfig)
	if err != nil {
		logError("Error reading syncConfig:" + err.Error())
	}
	// 创建结构体实例，用于存储解析后的JSON数据
	var data SyncConfig

	// 将JSON数据反序列化到结构体中
	err = json.Unmarshal(jsonData, &data)
	if err != nil {
		logError("Error unmarshalling JSON:" + err.Error())
	}
	return &data
}
func sync(client *kubernetes.Clientset, config *SyncConfig) {
	for _, item := range config.Certs {
		secret, err := client.CoreV1().Secrets(item.SecretNameSapce).Get(context.TODO(), item.SecretName, metav1.GetOptions{})
		if err != nil {
			logError("Get secret faild" + " namespace:" + item.SecretNameSapce + " secretName:" + item.SecretName)

		}
		crtValue, KeyValue := getCertsStr(secret)
		if len(crtValue) == 0 || len(KeyValue) == 0 {
			logError("Secret inviad" + " namespace:" + item.SecretNameSapce + " secretName:" + item.SecretName)
		}
		crtFilePath := item.CertPath + ".crt"
		keyFilePath := item.CertPath + ".key"
		if shouldRenew(string(crtValue), crtFilePath) || shouldRenew(string(KeyValue), keyFilePath) {
			renewCerts(crtValue, crtFilePath, KeyValue, keyFilePath, config)
		} else {
			log.Logger.Info("Certs are all Newest")
		}
	}
}

func getCertsStr(secret *v1.Secret) ([]byte, []byte) {
	var crtValue, KeyValue []byte
	for k := range secret.Data {
		if strings.HasSuffix(k, "crt") {
			crtValue = secret.Data[k]
		}

		if strings.HasSuffix(k, "key") {
			KeyValue = secret.Data[k]
		}

	}
	return crtValue, KeyValue
}

func renewCerts(crtValue []byte, crtFilePath string, KeyValue []byte, keyFilePath string, config *SyncConfig) {
	writeFile(crtValue, crtFilePath)
	writeFile(KeyValue, keyFilePath)
	renewTimes.Inc()
	isSucceed, _ := runNginxCmd(config.NginxTestCmd)
	if isSucceed {
		log.Logger.Info("Nginx test successfully")
		isSucceed, _ = runNginxCmd(config.NginxReloadCmd)
		if isSucceed {
			log.Logger.Info("Nginx reload successfully")
		} else {
			logError("Nginx reload faild")
		}
	} else {
		logError("Nginx test faild")
	}
}

func shouldRenew(crtValue, filePath string) bool {
	crtData, err := ioutil.ReadFile(filePath)
	if err != nil {
		if _, ok := err.(*fs.PathError); ok {
			return true
		} else {
			panic("Error reading syncConfig:" + err.Error())
		}
	}
	text := string(crtData)
	return text != crtValue
}

func writeFile(fileBytes []byte, filePath string) {
	err := os.MkdirAll(path.Dir(filePath), 0770)
	if err != nil {
		if !errors.Is(err, os.ErrExist) {
			logError("Create Directory faild path: " + path.Dir(filePath))
		}
	}
	err = ioutil.WriteFile(filePath, fileBytes, 0644)
	if err != nil {
		logError("Write " + filePath + " faild")
	}
}

func logError(message string) {
	certSyncToolStatus.Inc()
	log.Logger.Error(message)
}

func runNginxCmd(command string) (bool, string) {
	if debugNginx {
		log.Logger.Info(fmt.Sprintf("Running nginx command : %s", command))
		return true, fmt.Sprintf("Output of command: %s", command)
	} else {
		return cmd.Execute(command)
	}
}
