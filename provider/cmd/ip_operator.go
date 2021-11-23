package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	clusterutil "github.com/ovrclk/akash/provider/cluster/util"
	"golang.org/x/sync/errgroup"
	kubeErrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"net/http"
	"strings"

	"errors"
	"fmt"
	"github.com/ovrclk/akash/provider/cluster"
	clusterClient "github.com/ovrclk/akash/provider/cluster/kube"
	ctypes "github.com/ovrclk/akash/provider/cluster/types"
	mtypes "github.com/ovrclk/akash/x/market/types/v1beta2"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/tendermint/tendermint/libs/log"

	"time"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type managedIp struct {
	presentLease mtypes.LeaseID
	presentServiceName string
	lastEvent ctypes.IPResourceEvent
	presentSharingKey string
	presentExternalPort uint32
	presentPort uint32
	lastChangedAt time.Time
}

type ipOperator struct {
	state map[string]managedIp
	client cluster.Client
	log log.Logger
	server *operatorHttp
	leasesIgnored *ignoreList
	flagState prepareFlagFn
	flagIgnoredLeases prepareFlagFn
}

func (op *ipOperator) monitorUntilError(parentCtx context.Context) error {
	ctx, cancel := context.WithCancel(parentCtx)
	op.log.Info("starting observation")

	op.state = make(map[string]managedIp)

	entries, err := op.client.GetIPPassthroughs(ctx)
	if err != nil {
		cancel()
		return err
	}
	startupTime := time.Now()
	for _, ipPassThrough := range entries {
		k := getStateKey(ipPassThrough.GetLeaseID(), ipPassThrough.GetSharingKey(), ipPassThrough.GetExternalPort())
		op.state[k] = managedIp{
			presentLease:       ipPassThrough.GetLeaseID(),
			presentServiceName: ipPassThrough.GetServiceName(),
			lastEvent:          nil,
			presentSharingKey: ipPassThrough.GetSharingKey(),
			presentExternalPort: ipPassThrough.GetExternalPort(),
			presentPort: ipPassThrough.GetPort(),
			lastChangedAt:  startupTime,
		}
	}
	op.flagState()

	events, err := op.client.ObserveIPState(ctx)
	if err != nil {
		cancel()
		return err
	}

	var exitError error

	pruneTicker := time.NewTicker(2 * time.Minute /*op.cfg.pruneInterval*/)
	defer pruneTicker.Stop()
	prepareTicker := time.NewTicker(2 * time.Second /*op.cfg.webRefreshInterval*/)
	defer prepareTicker.Stop()

loop:
	for {
		select {
		case <-ctx.Done():
			exitError = ctx.Err()
			break loop

		case ev, ok := <-events:
			if !ok {
				exitError = errObservationStopped
				break loop
			}
			err = op.applyEvent(ctx, ev)
			if err != nil {
				op.log.Error("failed applying event", "err", err)
				exitError = err
				break loop
			}
		case <-pruneTicker.C:
			op.leasesIgnored.prune()
			op.flagIgnoredLeases()
		case <-prepareTicker.C:
			if err := op.server.prepareAll(); err != nil {
				op.log.Error("preparing web data failed", "err", err)
			}
		}
	}

	cancel()
	op.log.Info("ip operator done")
	return exitError
}

func (op *ipOperator) recordEventError(ev ctypes.IPResourceEvent, failure error) {
	// ff no error, no action
	if failure == nil {
		return
	}

	// check the error, only consider errors that are obviously
	// indicating a missing resource
	// otherwise simple errors like network issues could wind up with all CRDs
	// being ignored

	mark := false

	if kubeErrors.IsNotFound(failure) {
		mark = true
	}

	if errors.Is(failure, errExpectedResourceNotFound) {
		mark = true
	}

	errStr := failure.Error()
	// unless the error indicates a resource was not found, no action
	if strings.Contains(errStr, "not found") {
		mark = true
	}

	if !mark {
		return
	}

	op.log.Info("recording error for", "lease", ev.GetLeaseID().String(), "err", failure)
	op.leasesIgnored.addError(ev.GetLeaseID(), failure, ev.GetSharingKey())
	op.flagIgnoredLeases()
}

func (op *ipOperator) applyEvent(ctx context.Context, ev ctypes.IPResourceEvent) error {
	op.log.Debug("apply event", "event-type", ev.GetEventType(), "lease", ev.GetLeaseID())
	switch ev.GetEventType() {
	case ctypes.ProviderResourceDelete:
		// note that on delete the resource might be gone anyways because the namespace is deleted
		return op.applyDeleteEvent(ctx, ev)
	case ctypes.ProviderResourceAdd, ctypes.ProviderResourceUpdate:
		if op.leasesIgnored.isFlagged(ev.GetLeaseID()) {
			op.log.Info("ignoring event for", "lease", ev.GetLeaseID().String())
			return nil
		}
		err := op.applyAddOrUpdateEvent(ctx, ev)
		op.recordEventError(ev, err)
		return err
	default:
		return fmt.Errorf("%w: unknown event type %v", errObservationStopped, ev.GetEventType())
	}

}

func (op *ipOperator) applyDeleteEvent(ctx context.Context, ev ctypes.IPResourceEvent) error {
	directive := buildIPDirective(ev)
	err := op.client.PurgeIPPassthrough(ctx, ev.GetLeaseID(), directive)

	if err == nil {
		uid := getStateKeyFromEvent(ev)
		delete(op.state, uid)
		op.flagState()
	}

	return err
}

func buildIPDirective(ev ctypes.IPResourceEvent) ctypes.ClusterIPPassthroughDirective {
	return ctypes.ClusterIPPassthroughDirective{
		LeaseID:     ev.GetLeaseID(),
		ServiceName: ev.GetServiceName(),
		Port: ev.GetPort(),
		ExternalPort: ev.GetExternalPort(),
		SharingKey:  ev.GetSharingKey(),
		Protocol:  ev.GetProtocol(),
	}
}

func getStateKey(leaseID mtypes.LeaseID, sharingKey string, externalPort uint32) string {
	return fmt.Sprintf("%v-%s-%d", leaseID, sharingKey, externalPort)
}

func getStateKeyFromEvent(ev ctypes.IPResourceEvent) string{
	return getStateKey(ev.GetLeaseID(), ev.GetSharingKey(), ev.GetExternalPort())
}

func (op *ipOperator) applyAddOrUpdateEvent(ctx context.Context, ev ctypes.IPResourceEvent) error {
	leaseID := ev.GetLeaseID()

	uid := getStateKeyFromEvent(ev)

	op.log.Debug("connecting",
		"lease", leaseID,
		"service", ev.GetServiceName(),
		"externalPort", ev.GetExternalPort())
	entry, exists := op.state[uid]

	directive := buildIPDirective(ev)

	var err error
	shouldConnect := false

	if !exists {
		shouldConnect = true
		op.log.Debug("ip passthrough is new, applying")
		// Check to see if port or service name is different
	} else {
		hasChanged := entry.presentServiceName != ev.GetServiceName() ||
			entry.presentPort != ev.GetPort() ||
			entry.presentSharingKey != ev.GetSharingKey() ||
			entry.presentExternalPort != ev.GetExternalPort()
		if hasChanged {
			shouldConnect = true
			op.log.Debug("ip passthrough has changed, applying")
		}
	}

	if shouldConnect {
		op.log.Debug("Updating ip passthrough")
		err = op.client.CreateIPPassthrough(ctx, leaseID, directive)
	}


	if err == nil { // Update stored entry if everything went OK
		entry.presentServiceName = ev.GetServiceName()
		entry.presentLease = leaseID
		entry.lastEvent = ev
		entry.presentExternalPort = ev.GetExternalPort()
		entry.presentSharingKey = ev.GetSharingKey()
		entry.presentPort = ev.GetPort()
		entry.lastChangedAt = time.Now()
		op.state[uid] = entry
		op.flagState()
	}

	return err
}

func (op *ipOperator) webRouter() http.Handler {
	return op.server.router
}


func (op *ipOperator) prepareIgnoredLeases(pd *preparedResult) error {
	op.log.Debug("preparing ignore-list")
	return op.leasesIgnored.prepare(pd)
}

func (op *ipOperator) prepareState(pd *preparedResult) error {

	results := make(map[string]interface{})
	for _, managedIpEntry := range op.state {
		leaseID := managedIpEntry.presentLease

		k := getStateKey(leaseID, managedIpEntry.presentSharingKey, managedIpEntry.presentExternalPort)

		// TODO - add the resource name in kubernetes, for diagnostic reasons
		result := struct{
			LastEventTime string `json:"last-event-time,omitempty"`
			LeaseID      mtypes.LeaseID
			Namespace    string // diagnostic only
			Port uint32
			ExternalPort uint32
			ServiceName  string
			LastUpdate   string
		}{

			LeaseID:      leaseID,
			Namespace:    clusterutil.LeaseIDToNamespace(leaseID),
			Port:         managedIpEntry.presentPort,
			ExternalPort: managedIpEntry.presentExternalPort,
			ServiceName:  managedIpEntry.presentServiceName,
			LastUpdate:   managedIpEntry.lastChangedAt.String(),
		}

		results[k] = result
	}

	buf := &bytes.Buffer{}
	enc := json.NewEncoder(buf)
	err := enc.Encode(results)
	if err != nil {
		return err
	}

	pd.set(buf.Bytes())
	return nil
}

func newIpOperator(logger log.Logger, client cluster.Client, ilc ignoreListConfig) (*ipOperator) {
	retval := &ipOperator{
		state:  make(map[string]managedIp),
		client: client,
		log:    logger,
		server: newOperatorHttp(),
		leasesIgnored: newIgnoreList(ilc),
	}

	retval.flagState = retval.server.addPreparedEndpoint("/state", retval.prepareState)
	retval.flagIgnoredLeases = retval.server.addPreparedEndpoint("/state", retval.leasesIgnored.prepare)

	var kc kubernetes.Interface

	retval.server.router.HandleFunc("/reservations", func(rw http.ResponseWriter, req *http.Request){
		clientId := req.Header.Get("X-Client-Id")
		tr := authv1.TokenReview{
			Spec: authv1.TokenReviewSpec{
				Token: clientId,
			},
		}
		result, err := kc.AuthenticationV1().TokenReviews().Create(req.Context(), &tr, metav1.CreateOptions{})
		if err != nil {
			panic(err)
		}

		if ! result.Status.Authenticated {
			panic("not authed")
		}

		userInfo := result.Status.User
		_ = userInfo.Extra

	}).Methods(http.MethodGet, http.MethodPut)
	return retval
}

func doIPOperator(cmd *cobra.Command) error {
	ns := viper.GetString(FlagK8sManifestNS)
	listenAddr := viper.GetString(FlagListenAddress)
	logger := openLogger().With("operator","ip")

	// Config path not provided because the authorization comes from the role assigned to the deployment
	// and provided by kubernetes
	client, err := clusterClient.NewClient(logger, ns, "")
	if err != nil {
		return err
	}

	op := newIpOperator(logger, client, ignoreListConfigFromViper())

	router := op.webRouter()
	group, ctx := errgroup.WithContext(cmd.Context())

	group.Go(func() error {
		srv := http.Server{Addr: listenAddr, Handler: router}
		go func() {
			<-ctx.Done()
			_ = srv.Close()
		}()
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	})

	group.Go(func() error {
		return op.run(ctx)
	})

	err = group.Wait()
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func IPOperatorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "ip-operator",
		Short:        "kubernetes operator interfacing with Metal LB",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return doIPOperator(cmd)
		},
	}
	addOperatorFlags(cmd)

	return cmd
}

func (op *ipOperator) run(parentCtx context.Context) error {
	op.log.Debug("ip operator start")
	const threshold = 3 * time.Second
	for {
		lastAttempt := time.Now()
		err := op.monitorUntilError(parentCtx)
		if errors.Is(err, context.Canceled) {
			op.log.Debug("ip operator terminate")
			return err
		}

		op.log.Error("observation stopped", "err", err)

		// don't spin if there is a condition causing fast failure
		elapsed := time.Since(lastAttempt)
		if elapsed < threshold {
			op.log.Info("delaying")
			select {
			case <-parentCtx.Done():
				return parentCtx.Err()
			case <-time.After(threshold):
				// delay complete
			}
		}
	}
}
