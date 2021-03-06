package nsm

import (
	"context"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"

	local_connection "github.com/networkservicemesh/networkservicemesh/controlplane/pkg/apis/local/connection"
	"github.com/networkservicemesh/networkservicemesh/controlplane/pkg/apis/nsm"
	"github.com/networkservicemesh/networkservicemesh/controlplane/pkg/apis/registry"
	remote_connection "github.com/networkservicemesh/networkservicemesh/controlplane/pkg/apis/remote/connection"
	"github.com/networkservicemesh/networkservicemesh/controlplane/pkg/model"
	"github.com/networkservicemesh/networkservicemesh/controlplane/pkg/serviceregistry"
)

type networkServiceHealProcessor interface {
	healDstDown(healID string, connection *model.ClientConnection) bool
	healDataplaneDown(healID string, connection *model.ClientConnection) bool
	healDstUpdate(healID string, connection *model.ClientConnection) bool
	healDstNmgrDown(healID string, connection *model.ClientConnection) bool
}

type nsmHealProcessor struct {
	serviceRegistry serviceregistry.ServiceRegistry
	model           model.Model
	properties      *nsm.NsmProperties

	conManager connectionManager
	nseManager networkServiceEndpointManager
}

type connectionManager interface {
	request(ctx context.Context, request nsm.NSMRequest, existingConnection *model.ClientConnection) (nsm.NSMConnection, error)
	close(ctx context.Context, clientConnection *model.ClientConnection, closeDataplane bool, modelRemove bool) error
}

func (p *nsmHealProcessor) healDstDown(healID string, connection *model.ClientConnection) bool {
	logrus.Infof("NSM_Heal(1.1.1-%v) Checking if DST die is NSMD/DST die...", healID)
	// Check if this is a really HealStateDstDown or HealStateDstNmgrDown
	if !p.nseManager.isLocalEndpoint(connection.Endpoint) {
		ctx, cancel := context.WithTimeout(context.Background(), p.properties.HealTimeout*3)
		defer cancel()
		remoteNsmClient, err := p.nseManager.createNSEClient(ctx, connection.Endpoint)
		if remoteNsmClient != nil {
			_ = remoteNsmClient.Cleanup()
		}
		if err != nil {
			// This is NSMD die case.
			logrus.Infof("NSM_Heal(1.1.2-%v) Connection healing state is %v...", healID, nsm.HealStateDstNmgrDown)
			return p.healDstNmgrDown(healID, connection)
		}
	}

	logrus.Infof("NSM_Heal(1.1.2-%v) Connection healing state is %v...", healID, nsm.HealStateDstDown)

	// Destination is down, we need to find it again.
	if connection.Xcon.GetRemoteSource() != nil {
		// NSMd id remote one, we just need to close and return.
		logrus.Infof("NSM_Heal(2.1-%v) Remote NSE heal is done on source side", healID)
		return false
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), p.properties.HealTimeout*3)
		defer cancel()

		logrus.Infof("NSM_Heal(2.2-%v) Starting DST Heal...", healID)
		// We are client NSMd, we need to try recover our connection srv.

		endpointName := connection.Endpoint.GetNetworkserviceEndpoint().GetEndpointName()
		// Wait for NSE not equal to down one, since we know it will be re-registered with new EndpointName.
		if !p.waitNSE(ctx, connection, endpointName, connection.GetNetworkService(), p.nseIsNewAndAvailable) {
			// Not remote NSE found, we need to update connection
			if dst := connection.Xcon.GetRemoteDestination(); dst != nil {
				dst.SetId("-") // We need to mark this as new connection.
			}
			if dst := connection.Xcon.GetLocalDestination(); dst != nil {
				dst.SetId("-") // We need to mark this as new connection.
			}
		}
		// Fallback to heal with choose of new NSE.
		requestCtx, requestCancel := context.WithTimeout(context.Background(), p.properties.HealRequestTimeout)
		defer requestCancel()
		logrus.Errorf("NSM_Heal(2.3.0-%v) Starting Heal by calling request: %v", healID, connection.Request)

		recoveredConnection, err := p.conManager.request(requestCtx, connection.Request, connection)
		if err != nil {
			logrus.Errorf("NSM_Heal(2.3.1-%v) Failed to heal connection: %v", healID, err)
			return false
		} else {
			logrus.Infof("NSM_Heal(2.4-%v) Heal: Connection recovered: %v", healID, recoveredConnection)
			return true
		}
	}
}

func (p *nsmHealProcessor) healDataplaneDown(healID string, cc *model.ClientConnection) bool {
	ctx, cancel := context.WithTimeout(context.Background(), p.properties.HealTimeout)
	defer cancel()

	// Dataplane is down, we only need to re-programm dataplane.
	// 1. Wait for dataplane to appear.
	logrus.Infof("NSM_Heal(3.1-%v) Waiting for Dataplane to recovery...", healID)
	if err := p.serviceRegistry.WaitForDataplaneAvailable(p.model, p.properties.HealDataplaneTimeout); err != nil {
		logrus.Errorf("NSM_Heal(3.1-%v) Dataplane is not available on recovery for timeout %v: %v", p.properties.HealDataplaneTimeout, healID, err)
		return false
	}
	logrus.Infof("NSM_Heal(3.2-%v) Dataplane is now available...", healID)

	p.model.ApplyClientConnectionChanges(cc.GetID(), func(cc *model.ClientConnection) {
		if cc.Xcon.GetRemoteSource() != nil {
			cc.Xcon.GetRemoteSource().State = remote_connection.State_DOWN
		} else if cc.Xcon.GetLocalSource() != nil {
			cc.Xcon.GetLocalSource().State = local_connection.State_DOWN
		}
	})

	if cc.Xcon.GetRemoteSource() != nil {
		// NSMd id remote one, we just need to close and return.
		// Recovery will be performed by NSM client side.
		logrus.Infof("NSM_Heal(3.3-%v)  Healing will be continued on source side...", healID)
		return true
	}

	// We have Dataplane now, let's try request all again.
	// Update request to contain a proper connection object from previous attempt.
	request := cc.Request.Clone()
	request.SetConnection(cc.GetConnectionSource())
	p.requestOrClose(fmt.Sprintf("NSM_Heal(3.4-%v) ", healID), ctx, request, cc)
	return true
}

func (p *nsmHealProcessor) healDstUpdate(healID string, cc *model.ClientConnection) bool {
	ctx, cancel := context.WithTimeout(context.Background(), p.properties.HealTimeout)
	defer cancel()

	// Destination is updated.
	// Update request to contain a proper connection object from previous attempt.
	logrus.Infof("NSM_Heal(5.1-%v) Healing Src Update... %v", healID, cc)
	if cc.Request != nil {
		request := cc.Request.Clone()
		request.SetConnection(cc.GetConnectionSource())

		p.requestOrClose(fmt.Sprintf("NSM_Heal(5.2-%v) ", healID), ctx, request, cc)
		return true
	}
	return false
}

func (p *nsmHealProcessor) healDstNmgrDown(healID string, connection *model.ClientConnection) bool {
	logrus.Infof("NSM_Heal(6.1-%v) Starting DST + NSMGR Heal...", healID)

	ctx, cancel := context.WithTimeout(context.Background(), p.properties.HealTimeout*3)
	defer cancel()

	networkService := connection.GetNetworkService()

	var endpointName string
	// Wait for exact same NSE to be available with NSMD connection alive.
	if connection.Endpoint != nil {
		endpointName = connection.Endpoint.GetNetworkserviceEndpoint().GetEndpointName()
		if !p.waitNSE(ctx, connection, endpointName, networkService, p.nseIsSameAndAvailable) {
			// Not remote NSE found, we need to update connection
			if dst := connection.Xcon.GetRemoteDestination(); dst != nil {
				dst.SetId("-") // We need to mark this as new connection.
			}
			if dst := connection.Xcon.GetLocalDestination(); dst != nil {
				dst.SetId("-") // We need to mark this as new connection.
			}
		}
	}
	requestCtx, requestCancel := context.WithTimeout(context.Background(), p.properties.HealRequestTimeout)
	defer requestCancel()
	recoveredConnection, err := p.conManager.request(requestCtx, connection.Request, connection)
	if err != nil {
		logrus.Errorf("NSM_Heal(6.2.1-%v) Failed to heal connection with same NSE from registry: %v", healID, err)
		if endpointName != "" {
			logrus.Infof("NSM_Heal(6.2.2-%v) Waiting for another NSEs...", healID)
			// In this case, most probable both NSMD and NSE are die, and registry was outdated on moment of waitNSE.
			if p.waitNSE(ctx, connection, endpointName, networkService, p.nseIsNewAndAvailable) {
				// Ok we have NSE, lets retry request
				requestCtx, requestCancel := context.WithTimeout(context.Background(), p.properties.HealRequestTimeout)
				defer requestCancel()
				recoveredConnection, err = p.conManager.request(requestCtx, connection.Request, connection)
				if err != nil {
					if err != nil {
						logrus.Errorf("NSM_Heal(6.2.3-%v) Error in Recovery Close: %v", healID, err)
					}
				} else {
					logrus.Infof("NSM_Heal(6.3-%v) Heal: Connection recovered: %v", healID, recoveredConnection)
					return true
				}
			}
		}

		logrus.Errorf("NSM_Heal(6.4.1-%v) Failed to heal connection: %v", healID, err)
		// We need to delete connection, since we are not able to Heal it
		p.model.DeleteClientConnection(connection.ConnectionID)
		if err != nil {
			logrus.Errorf("NSM_Heal(6.4.2-%v) Error in Recovery Close: %v", healID, err)
		}
	} else {
		logrus.Infof("NSM_Heal(6.5-%v) Heal: Connection recovered: %v", healID, recoveredConnection)
		return true
	}
	return false
}

func (p *nsmHealProcessor) requestOrClose(logPrefix string, ctx context.Context, request nsm.NSMRequest, clientConnection *model.ClientConnection) {
	logrus.Infof("%v delegate to Request %v", logPrefix, request)
	connection, err := p.conManager.request(ctx, request, clientConnection)
	if err != nil {
		logrus.Errorf("%v Failed to heal connection: %v", logPrefix, err)
		// Close in case of any errors in recovery.
		if err = p.conManager.close(context.Background(), clientConnection, true, true); err != nil {
			logrus.Errorf("%v Error in Recovery Close: %v", logPrefix, err)
		}
	} else {
		logrus.Infof("%v Heal: Connection recovered: %v", logPrefix, connection)
	}
}

type nseValidator func(ctx context.Context, endpoint string, reg *registry.NSERegistration) bool

func (p *nsmHealProcessor) nseIsNewAndAvailable(ctx context.Context, endpointName string, reg *registry.NSERegistration) bool {
	if endpointName != "" && reg.GetNetworkserviceEndpoint().GetEndpointName() == endpointName {
		// Skip ignored endpoint
		return false
	}

	// Check local only if not waiting for specific NSE.
	if p.model.GetNsm().GetName() == reg.GetNetworkServiceManager().GetName() {
		// Another local endpoint is found, success.
		return true
	}

	// Check remote is accessible.
	if p.nseManager.checkUpdateNSE(ctx, reg) {
		logrus.Infof("NSE is available and Remote NSMD is accessible. %s.", reg.NetworkServiceManager.Url)
		// We are able to connect to NSM with required NSE
		return true
	}

	return false
}

func (p *nsmHealProcessor) nseIsSameAndAvailable(ctx context.Context, endpointName string, reg *registry.NSERegistration) bool {
	if reg.GetNetworkserviceEndpoint().GetEndpointName() != endpointName {
		return false
	}

	// Our endpoint, we need to check if it is remote one and NSM is accessible.

	// Check remote is accessible.
	if p.nseManager.checkUpdateNSE(ctx, reg) {
		logrus.Infof("NSE is available and Remote NSMD is accessible. %s.", reg.NetworkServiceManager.Url)
		// We are able to connect to NSM with required NSE
		return true
	}

	return false
}

func (p *nsmHealProcessor) waitNSE(ctx context.Context, clientConnection *model.ClientConnection, endpointName, networkService string,
	nseValidator nseValidator) bool {

	discoveryClient, err := p.serviceRegistry.DiscoveryClient()
	if err != nil {
		logrus.Errorf("Failed to connect to Registry... %v", err)
		// Still try to recovery
		return false
	}

	st := time.Now()

	nseRequest := &registry.FindNetworkServiceRequest{
		NetworkServiceName: networkService,
	}

	defer func() {
		logrus.Infof("Complete Waiting for Remote NSE/NSMD with network service %s. Since elapsed: %v", networkService, time.Since(st))
	}()

	for {
		logrus.Infof("NSM: RemoteNSE: Waiting for NSE with network service %s. Since elapsed: %v", networkService, time.Since(st))

		endpointResponse, err := discoveryClient.FindNetworkService(ctx, nseRequest)
		if err == nil {
			for _, ep := range endpointResponse.NetworkServiceEndpoints {
				reg := &registry.NSERegistration{
					NetworkServiceManager:  endpointResponse.GetNetworkServiceManagers()[ep.GetNetworkServiceManagerName()],
					NetworkserviceEndpoint: ep,
					NetworkService:         endpointResponse.GetNetworkService(),
				}

				if nseValidator(ctx, endpointName, reg) {
					clientConnection.Endpoint = reg
					return true
				}
			}
		}

		if time.Since(st) > p.properties.HealDSTNSEWaitTimeout {
			logrus.Errorf("Timeout waiting for NetworkService: %v", networkService)
			return false
		}
		// Wait a bit
		<-time.After(p.properties.HealDSTNSEWaitTick)
	}
}
