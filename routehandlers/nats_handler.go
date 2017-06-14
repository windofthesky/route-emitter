package routehandlers

import (
	"errors"

	"code.cloudfoundry.org/bbs/models"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/route-emitter/emitter"
	"code.cloudfoundry.org/route-emitter/routingtable"
	"code.cloudfoundry.org/route-emitter/routingtable/schema/endpoint"
	"code.cloudfoundry.org/route-emitter/routingtable/util"
	"code.cloudfoundry.org/route-emitter/watcher"
	"code.cloudfoundry.org/runtimeschema/metric"
)

var (
	routesTotal  = metric.Metric("RoutesTotal")
	routesSynced = metric.Counter("RoutesSynced")

	routesRegistered   = metric.Counter("RoutesRegistered")
	routesUnregistered = metric.Counter("RoutesUnregistered")

	httpRouteCount = metric.Metric("HTTPRouteCount")
	tcpRouteCount  = metric.Metric("TCPRouteCount")
)

type NATSHandler struct {
	routingTable      routingtable.RoutingTable
	natsEmitter       emitter.NATSEmitter
	routingAPIEmitter emitter.RoutingAPIEmitter
	localMode         bool
}

var _ watcher.RouteHandler = new(NATSHandler)

func NewNATSHandler(routingTable routingtable.RoutingTable, natsEmitter emitter.NATSEmitter, routingAPIEmitter emitter.RoutingAPIEmitter, localMode bool) *NATSHandler {
	return &NATSHandler{
		routingTable:      routingTable,
		natsEmitter:       natsEmitter,
		routingAPIEmitter: routingAPIEmitter,
		localMode:         localMode,
	}
}

func (handler *NATSHandler) HandleEvent(logger lager.Logger, event models.Event) {
	switch event := event.(type) {
	case *models.DesiredLRPCreatedEvent:
		desiredInfo := event.DesiredLrp.DesiredLRPSchedulingInfo()
		handler.handleDesiredCreate(logger, &desiredInfo)
	case *models.DesiredLRPChangedEvent:
		before := event.Before.DesiredLRPSchedulingInfo()
		after := event.After.DesiredLRPSchedulingInfo()
		handler.handleDesiredUpdate(logger, &before, &after)
	case *models.DesiredLRPRemovedEvent:
		desiredInfo := event.DesiredLrp.DesiredLRPSchedulingInfo()
		handler.handleDesiredDelete(logger, &desiredInfo)
	case *models.ActualLRPCreatedEvent:
		routingInfo := endpoint.NewActualLRPRoutingInfo(event.ActualLrpGroup)
		handler.handleActualCreate(logger, routingInfo)
	case *models.ActualLRPChangedEvent:
		before := endpoint.NewActualLRPRoutingInfo(event.Before)
		after := endpoint.NewActualLRPRoutingInfo(event.After)
		handler.handleActualUpdate(logger, before, after)
	case *models.ActualLRPRemovedEvent:
		routingInfo := endpoint.NewActualLRPRoutingInfo(event.ActualLrpGroup)
		handler.handleActualDelete(logger, routingInfo)
	default:
		logger.Error("did-not-handle-unrecognizable-event", errors.New("unrecognizable-event"), lager.Data{"event-type": event.EventType()})
	}
}

func (handler *NATSHandler) Emit(logger lager.Logger) {
	routingEvents, messagesToEmit := handler.routingTable.Emit()

	logger.Info("emitting-nats-messages", lager.Data{"messages": messagesToEmit})
	if handler.natsEmitter != nil {
		err := handler.natsEmitter.Emit(messagesToEmit)
		if err != nil {
			logger.Error("failed-to-emit-nats-routes", err)
		}
	}

	logger.Info("emitting-routing-api-messages", lager.Data{"messages": routingEvents})
	if handler.routingAPIEmitter != nil {
		err := handler.routingAPIEmitter.Emit(routingEvents)
		if err != nil {
			logger.Error("failed-to-emit-tcp-routes", err)
		}
	}

	routesSynced.Add(messagesToEmit.RouteRegistrationCount())
	err := routesTotal.Send(handler.routingTable.HTTPEndpointCount())
	if err != nil {
		logger.Error("failed-to-send-http-route-count-metric", err)
	}
}

func (handler *NATSHandler) Sync(
	logger lager.Logger,
	desired []*models.DesiredLRPSchedulingInfo,
	actuals []*endpoint.ActualLRPRoutingInfo,
	domains models.DomainSet,
	cachedEvents map[string]models.Event,
) {
	logger = logger.Session("nats-sync")
	logger.Debug("starting")
	defer logger.Debug("completed")

	newTable := routingtable.NewRoutingTable(logger, false)

	for _, lrp := range desired {
		newTable.SetRoutes(nil, lrp)
	}

	for _, lrp := range actuals {
		newTable.AddEndpoint(lrp)
	}

	/////////

	natsEmitter := handler.natsEmitter
	routingAPIEmitter := handler.routingAPIEmitter
	table := handler.routingTable

	handler.natsEmitter = nil
	handler.routingAPIEmitter = nil
	handler.routingTable = newTable

	for _, event := range cachedEvents {
		handler.HandleEvent(logger, event)
	}

	handler.routingTable = table
	handler.natsEmitter = natsEmitter
	handler.routingAPIEmitter = routingAPIEmitter

	//////////

	routeMappings, messages := handler.routingTable.Swap(newTable, domains)
	logger.Debug("start-emitting-messages", lager.Data{
		"num-registration-messages":   len(messages.RegistrationMessages),
		"num-unregistration-messages": len(messages.UnregistrationMessages),
	})
	handler.emitMessages(logger, messages, routeMappings)
	logger.Debug("done-emitting-messages", lager.Data{
		"num-registration-messages":   len(messages.RegistrationMessages),
		"num-unregistration-messages": len(messages.UnregistrationMessages),
	})

	if handler.localMode {
		err := httpRouteCount.Send(handler.routingTable.HTTPEndpointCount())
		if err != nil {
			logger.Error("failed-to-send-routes-total-metric", err)
		}
		err = tcpRouteCount.Send(handler.routingTable.TCPRouteCount())
		if err != nil {
			logger.Error("failed-to-send-tcp-route-count-metric", err)
		}
	}
}

func (handler *NATSHandler) RefreshDesired(logger lager.Logger, desiredInfo []*models.DesiredLRPSchedulingInfo) {
	for _, desiredLRP := range desiredInfo {
		routeMappings, messagesToEmit := handler.routingTable.SetRoutes(nil, desiredLRP)
		handler.emitMessages(logger, messagesToEmit, routeMappings)
	}
}

func (handler *NATSHandler) ShouldRefreshDesired(actual *endpoint.ActualLRPRoutingInfo) bool {
	return !handler.routingTable.HasExternalRoutes(actual)
}

func (handler *NATSHandler) handleDesiredCreate(logger lager.Logger, desiredLRP *models.DesiredLRPSchedulingInfo) {
	logger = logger.Session("handle-desired-create", util.DesiredLRPData(desiredLRP))
	logger.Info("starting")
	defer logger.Info("complete")
	routeMappings, messagesToEmit := handler.routingTable.SetRoutes(nil, desiredLRP)
	handler.emitMessages(logger, messagesToEmit, routeMappings)
}

func (handler *NATSHandler) handleDesiredUpdate(logger lager.Logger, before, after *models.DesiredLRPSchedulingInfo) {
	logger = logger.Session("handling-desired-update", lager.Data{
		"before": util.DesiredLRPData(before),
		"after":  util.DesiredLRPData(after),
	})
	logger.Info("starting")
	defer logger.Info("complete")

	routeMappings, messagesToEmit := handler.routingTable.SetRoutes(before, after)
	handler.emitMessages(logger, messagesToEmit, routeMappings)
}

func (handler *NATSHandler) handleDesiredDelete(logger lager.Logger, schedulingInfo *models.DesiredLRPSchedulingInfo) {
	logger = logger.Session("handling-desired-delete", util.DesiredLRPData(schedulingInfo))
	logger.Info("starting")
	defer logger.Info("complete")
	routeMappings, messagesToEmit := handler.routingTable.RemoveRoutes(schedulingInfo)
	handler.emitMessages(logger, messagesToEmit, routeMappings)
}

func (handler *NATSHandler) handleActualCreate(logger lager.Logger, actualLRPInfo *endpoint.ActualLRPRoutingInfo) {
	logger = logger.Session("handling-actual-create", util.ActualLRPData(actualLRPInfo))
	logger.Info("starting")
	defer logger.Info("complete")
	if actualLRPInfo.ActualLRP.State == models.ActualLRPStateRunning {
		logger.Info("handler-adding-endpoint", lager.Data{"net_info": actualLRPInfo.ActualLRP.ActualLRPNetInfo})
		routeMappings, messagesToEmit := handler.routingTable.AddEndpoint(actualLRPInfo)
		handler.emitMessages(logger, messagesToEmit, routeMappings)
	}
}

func (handler *NATSHandler) handleActualUpdate(logger lager.Logger, before, after *endpoint.ActualLRPRoutingInfo) {
	logger = logger.Session("handling-actual-update", lager.Data{
		"before": util.ActualLRPData(before),
		"after":  util.ActualLRPData(after),
	})
	logger.Info("starting")
	defer logger.Info("complete")

	var (
		messagesToEmit routingtable.MessagesToEmit
		routeMappings  routingtable.TCPRouteMappings
	)
	switch {
	case after.ActualLRP.State == models.ActualLRPStateRunning:
		logger.Info("handler-adding-endpoint", lager.Data{"net_info": after.ActualLRP.ActualLRPNetInfo})
		routeMappings, messagesToEmit = handler.routingTable.AddEndpoint(after)
	case after.ActualLRP.State != models.ActualLRPStateRunning && before.ActualLRP.State == models.ActualLRPStateRunning:
		logger.Info("handler-removing-endpoint", lager.Data{"net_info": before.ActualLRP.ActualLRPNetInfo})
		routeMappings, messagesToEmit = handler.routingTable.RemoveEndpoint(before)
	}
	handler.emitMessages(logger, messagesToEmit, routeMappings)
}

func (handler *NATSHandler) handleActualDelete(logger lager.Logger, actualLRPInfo *endpoint.ActualLRPRoutingInfo) {
	logger = logger.Session("handling-actual-delete", util.ActualLRPData(actualLRPInfo))
	logger.Info("starting")
	defer logger.Info("complete")
	if actualLRPInfo.ActualLRP.State == models.ActualLRPStateRunning {
		logger.Info("handler-removing-endpoint", lager.Data{"net_info": actualLRPInfo.ActualLRP.ActualLRPNetInfo})
		routeMappings, messagesToEmit := handler.routingTable.RemoveEndpoint(actualLRPInfo)
		handler.emitMessages(logger, messagesToEmit, routeMappings)
	}
}

type set map[interface{}]struct{}

func (set set) contains(value interface{}) bool {
	_, found := set[value]
	return found
}

func (set set) add(value interface{}) {
	set[value] = struct{}{}
}

func (handler *NATSHandler) emitMessages(logger lager.Logger, messagesToEmit routingtable.MessagesToEmit, routeMappings routingtable.TCPRouteMappings) {
	if handler.natsEmitter != nil {
		logger.Debug("emit-messages", lager.Data{"messages": messagesToEmit})
		err := handler.natsEmitter.Emit(messagesToEmit)
		if err != nil {
			logger.Error("failed-to-emit-http-routes", err)
		}
		routesRegistered.Add(messagesToEmit.RouteRegistrationCount())
		routesUnregistered.Add(messagesToEmit.RouteUnregistrationCount())
	} else {
		logger.Info("no-emitter-configured-skipping-emit-messages", lager.Data{"messages": messagesToEmit})
	}

	if handler.routingAPIEmitter != nil {
		err := handler.routingAPIEmitter.Emit(routeMappings)
		if err != nil {
			logger.Error("failed-to-emit-http-routes", err)
		}
	}
}
