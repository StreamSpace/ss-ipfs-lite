package lib

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"time"

	ipfslite "github.com/StreamSpace/ss-light-client"
	"github.com/StreamSpace/ss-light-client/scp/engine"
	externalip "github.com/glendc/go-external-ip"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	syncds "github.com/ipfs/go-datastore/sync"
	logger "github.com/ipfs/go-log/v2"
	crypto "github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/pnet"
	"github.com/multiformats/go-multiaddr"
)

var log = logger.Logger("ss_light")

// Constants
const (
	fpSeparator   string = string(os.PathSeparator)
	cmdSeparator  string = "%$#"
	apiAddr       string = "http://35.244.28.138:6343/v3/execute"
	peerThreshold int    = 5

	success       = 200
	internalError = 500
	timeoutError  = 504
	serviceError  = 503
)

// API objects
type cookie struct {
	Id            string
	Leaders       []peer.AddrInfo
	DownloadIndex string
	Filename      string
	Hash          string
	Link          string
}

type StatOut struct {
	ConnectedPeers []string
	Ledgers        []*engine.SSReceipt
	DownloadTime   int
}

type info struct {
	Cookie   cookie
	SwarmKey []byte
	Rate     string
}

type apiResp struct {
	Status  int    `json:"status"`
	Data    info   `json:"data"`
	Details string `json:"details"`
}

func (a *apiResp) UnmarshalJSON(b []byte) error {
	val := map[string]string{}
	tmp := struct {
		Status  int             `json:"status"`
		Details string          `json:"details"`
		Data    json.RawMessage `json:"data"`
	}{}
	log.Debugf("Raw response %s", string(b))
	err := json.Unmarshal(b, &val)
	if err != nil {
		return err
	}
	log.Debugf("API response %s", val["val"])
	err = json.Unmarshal([]byte(val["val"]), &tmp)
	if err != nil {
		return err
	}
	if tmp.Status != 200 {
		errStr := tmp.Details
		if len(errStr) == 0 {
			errStr = fmt.Sprintf("Invalid status from server: %s", tmp.Status)
		}
		return errors.New(errStr)
	}
	a.Status = tmp.Status
	return json.Unmarshal(tmp.Data, &a.Data)
}

func combineArgs(separator string, args ...string) (retPath string) {
	for idx, v := range args {
		if idx != 0 {
			retPath += separator
		}
		retPath += v
	}
	return
}

func getExternalIp() string {
	consensus := externalip.DefaultConsensus(nil, nil)
	ip, err := consensus.ExternalIP()
	if err != nil {
		return "0.0.0.0"
	}
	return ip.String()
}

func getInfo(sharable, oldCookie string, pubKey crypto.PubKey) (*info, error) {
	pubKB, _ := pubKey.Bytes()
	args := map[string]interface{}{
		"val": combineArgs(
			cmdSeparator,
			"hive",
			"customer",
			"fetch",
			sharable,
			"--public-key",
			base64.StdEncoding.EncodeToString(pubKB),
			"--source-ip",
			getExternalIp(),
			"-j",
		),
	}
	if len(oldCookie) > 0 {
		args["val"] = combineArgs(
			cmdSeparator,
			args["val"].(string),
			"--cookie",
			oldCookie,
		)
	}
	buf, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	resp, err := http.Post(apiAddr, "application/json", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBuf, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	respData := &apiResp{}
	err = json.Unmarshal(respBuf, &respData)
	if err != nil {
		log.Errorf("Failed unmarshaling result Err:%s Resp:%s", err.Error(), string(respBuf))
		return nil, err
	}
	return &respData.Data, nil
}

func updateInfo(i *info, timeConsumed int64) error {
	args := map[string]interface{}{
		"val": combineArgs(
			cmdSeparator,
			"hive",
			"customer",
			"complete",
			i.Cookie.Id,
			fmt.Sprintf("%d", timeConsumed),
			"-j",
		),
	}
	buf, err := json.Marshal(args)
	if err != nil {
		return err
	}
	resp, err := http.Post(apiAddr, "application/json", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBuf, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	respData := &apiResp{}
	err = json.Unmarshal(respBuf, &respData)
	if err != nil && respData.Status != 200 {
		return err
	}
	return nil
}

type LightClient struct {
	destination string
	repoRoot    string
	jsonOut     bool
	timeout     time.Duration

	privKey crypto.PrivKey
	pubKey  crypto.PubKey
	ds      datastore.Batching
}

func NewLightClient(
	destination string,
	timeout string,
	jsonOut bool,
) (*LightClient, error) {

	priv, pubk, err := crypto.GenerateKeyPair(crypto.Ed25519, 2048)
	if err != nil {
		log.Errorf("Failed generating key pair Err:%s", err.Error())
		return nil, err
	}

	ds := syncds.MutexWrap(datastore.NewMapDatastore())

	to, err := time.ParseDuration(timeout)
	if err != nil {
		log.Warn("Invalid timeout duration specified. Using default 15m")
		to = time.Minute * 45
	}

	return &LightClient{
		destination: destination,
		jsonOut:     jsonOut,
		timeout:     to,
		privKey:     priv,
		pubKey:      pubk,
		ds:          ds,
	}, nil
}

type ProgressUpdater interface {
	UpdateProgress(int, int, int)
}

func (l *LightClient) Start(
	sharable string,
	onlyInfo bool,
	stat bool,
	progUpd ProgressUpdater,
) *Out {
	metadata, err := getInfo(sharable, "", l.pubKey)
	if err != nil {
		log.Errorf("Failed getting metadata Err: %s", err.Error())
		return NewOut(serviceError, "Failed getting metadata", err.Error(), nil)
	}

	// STEP : Got metadata
	showStep(success, "Got metadata", l.jsonOut)

	log.Infof("Got metadata info %+v", metadata)
	if onlyInfo {
		return NewOut(success, MetaInfo, "", metadata)
	}
	if l.destination == "." {
		l.destination = combineArgs(fpSeparator, l.destination, metadata.Cookie.Filename)
	}
	dst, err := os.Create(l.destination)
	if err != nil {
		log.Errorf("Failed creating dest file Err: %s", err.Error())
		return NewOut(internalError, "Failed creating destination file", err.Error(), nil)
	}

	ctx, cancel := context.WithTimeout(context.Background(), l.timeout)
	defer cancel()

	psk, err := pnet.DecodeV1PSK(bytes.NewReader(metadata.SwarmKey))
	if err != nil {
		log.Errorf("Failed decoding swarm key Err: %s", err.Error())
		return NewOut(internalError, "Failed decoding swarm key provided", err.Error(), nil)
	}

	listenIP4, _ := multiaddr.NewMultiaddr("/ip4/0.0.0.0/tcp/45000")
	listenIP6, _ := multiaddr.NewMultiaddr("/ip6/::/tcp/45000")
	h, dht, err := ipfslite.SetupLibp2p(
		ctx,
		l.privKey,
		psk,
		[]multiaddr.Multiaddr{listenIP4, listenIP6},
		l.ds,
		ipfslite.Libp2pOptionsExtra...,
	)
	if err != nil {
		log.Errorf("Failed setting up libp2p node Err: %s", err.Error())
		return NewOut(internalError, "Failed setting up p2p peer", err.Error(), nil)
	}
	cfg := &ipfslite.Config{
		Mtdt: map[string]interface{}{
			"download_index": metadata.Cookie.DownloadIndex,
		},
		Rate: metadata.Rate,
	}
	lite, err := ipfslite.New(ctx, l.ds, h, dht, cfg)
	if err != nil {
		log.Errorf("Failed setting up p2p xfer node Err: %s", err.Error())
		return NewOut(internalError, "Failed setting up light client", err.Error(), nil)
	}

	// STEP : Download agent created
	showStep(success, "Download agent created", l.jsonOut)

	count := lite.Bootstrap(metadata.Cookie.Leaders)

	// STEP : Bootstrap done
	showStep(success, "Bootstrapped", l.jsonOut)

	if count < peerThreshold {
		go func() {
			start := time.Now()
			for count < peerThreshold {
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second * 30):
					if time.Since(start) > time.Minute*15 {
						log.Warn("Tried getting more peers for 15mins")
						showStep(timeoutError, "Download timed out", l.jsonOut)
						return
					}
					// Try to re-bootstrap if client was unable to bootstrap previously
					if count < len(metadata.Cookie.Leaders) {
						count += lite.Bootstrap(metadata.Cookie.Leaders)
						// STEP : Re-Bootstrap done
						showStep(success, "Re-Bootstrapped", l.jsonOut)
					}
				}
			}
			log.Infof("Done lagged bootstrapping. New count %d", count)
		}()
	}
	if count == 0 {
		log.Warn("No nodes connected. Waiting to find more")
		for {
			select {
			case <-ctx.Done():
				log.Info("Client stopped while waiting for more peers")
				return NewOut(internalError, "Stopped while waiting for peers", "context cancelled", nil)
			case <-time.After(time.Second):
				break
			}
			if count > 0 {
				break
			}
		}
	}
	log.Infof("Connected to %d peers. Starting download", count)

	c, err := cid.Decode(metadata.Cookie.Hash)
	if err != nil {
		log.Errorf("Failed decoding file hash Err: %s", err.Error())
		return NewOut(internalError, "Failed decoding filehash provided", err.Error(), nil)
	}

	// STEP : Starting Download
	showStep(success, "Starting download", l.jsonOut)

	startTime := time.Now().Unix()
	rsc, err := lite.GetFile(ctx, c)
	if err != nil {
		return NewOut(500, "Failed getting file", err.Error(), nil)
	}
	defer rsc.Close()

	if progUpd != nil {
		go func() {
			for {
				st, err := dst.Stat()
				if err == nil {
					prog := float64(st.Size()) / float64(rsc.Size()) * 100
					log.Infof("Updating progress %d", int(prog))
					progUpd.UpdateProgress(int(prog), int(st.Size()), int(rsc.Size()))
					if prog == 100 {
						log.Infof("Progress complete")
						return
					}
				}
				select {
				case <-ctx.Done():
					log.Warn("Stopping progress updated on context cancel")
				case <-time.After(time.Millisecond * 500):
					break
				}
			}
		}()
	}

	_, err = io.Copy(dst, rsc)
	if err != nil {
		return NewOut(internalError, "Failed writing to destination", err.Error(), nil)
	}
	downloadTime := time.Now().Unix() - startTime

	// STEP : Waiting for micropayments clean up
	showStep(success, "Finishing download", l.jsonOut)
	// Wait 5 secs for SCP to send all MPs. This can be optimized
	<-time.After(time.Second * 5)

	err = updateInfo(metadata, downloadTime)
	if err != nil {
		log.Warn("Failed updating metadata after download Err: %s", err.Error())
	}
	// STEP : Updated Cookie
	showStep(success, "Updating cookie", l.jsonOut)

	if !stat {
		return NewOut(200, DownloadSuccess, "", nil)
	}
	connectedPeers := []string{}
	for _, pID := range lite.Host.Network().Peers() {
		connectedPeers = append(connectedPeers, pID.String())
	}
	ledgers, _ := lite.Scp.GetMicroPayments()
	out := StatOut{
		ConnectedPeers: connectedPeers,
		Ledgers:        ledgers,
		DownloadTime:   int(downloadTime),
	}
	return NewOut(success, "Stats", "", out)
}

func showStep(status int, message string, jsonOut bool) {
	out := NewOut(success, message, "", nil)
	OutMessage(out, jsonOut)
}
