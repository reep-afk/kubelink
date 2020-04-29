/*
 * Copyright 2020 Mandelsoft. All rights reserved.
 *  This file is licensed under the Apache Software License, v. 2 except as noted
 *  otherwise in the LICENSE file
 *
 *  Licensed under the Apache License, Version 2.0 (the "License");
 *  you may not use this file except in compliance with the License.
 *  You may obtain a copy of the License at
 *
 *       http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 */

package broker

import (
	"fmt"
	"io"
	"net"
	"time"

	"github.com/gardener/controller-manager-library/pkg/certs"
	"github.com/gardener/controller-manager-library/pkg/controllermanager/controller/reconcile"
	"github.com/gardener/controller-manager-library/pkg/ctxutil"
	"github.com/gardener/controller-manager-library/pkg/logger"
	"github.com/gardener/controller-manager-library/pkg/resources"
	"github.com/vishvananda/netlink"

	"github.com/mandelsoft/kubelink/pkg/apis/kubelink/v1alpha1"
	"github.com/mandelsoft/kubelink/pkg/controllers"
	"github.com/mandelsoft/kubelink/pkg/kubelink"
	"github.com/mandelsoft/kubelink/pkg/tcp"
)

type reconciler struct {
	*controllers.Reconciler
	config   *Config
	certInfo *CertInfo
	mux      *Mux
}

var _ reconcile.Interface = &reconciler{}
var _ controllers.ReconcilerImplementation = &reconciler{}

///////////////////////////////////////////////////////////////////////////////

func (this *reconciler) Config(cfg interface{}) *controllers.Config {
	return &cfg.(*Config).Config
}

func (this *reconciler) Gateway(obj *v1alpha1.KubeLink) (net.IP, error) {
	gateway := this.NodeInterface().IP
	match, ip := this.config.MatchLink(obj)
	if !match {
		return nil, nil
	}
	return gateway, this.mux.GetError(ip)
}

func (this *reconciler) UpdateGateway(link *v1alpha1.KubeLink) *string {
	gateway := this.NodeInterface().IP
	match, _ := this.config.MatchLink(link)
	if !match {
		gateway = nil
	}

	if gateway != nil {
		s := gateway.String()
		return &s
	}
	return nil
}

func (this *reconciler) IsManagedRoute(route *netlink.Route, routes kubelink.Routes) bool {
	if route.LinkIndex == this.mux.tun.link.Attrs().Index {
		return true
	}
	if route.Dst != nil {
		for _, r := range routes {
			if tcp.EqualCIDR(route.Dst, r.Dst) {
				return true
			}
		}
	}
	return false
}

func (this *reconciler) RequiredRoutes() kubelink.Routes {
	routes := this.Links().GetRoutesToLink(this.NodeInterface(), this.mux.tun.link)
	return append(routes, netlink.Route{LinkIndex: this.mux.tun.link.Attrs().Index, Dst: this.config.ClusterCIDR})
}

///////////////////////////////////////////////////////////////////////////////

func (this *reconciler) Setup() {
	var err error
	var certificate certs.CertificateSource
	if this.config.CertFile != "" {
		certificate, err = this.CreateFileCertificateSource()
	} else {
		if this.config.Secret != "" {
			certificate, err = this.CreateSecretCertificateSource()
		}
	}
	if err != nil {
		panic(fmt.Errorf("cannot setup tls: %s", err))
	}

	this.Reconciler.Setup()
	tun, err := NewTun(this.Controller(), this.config.Interface, this.config.ClusterAddress, this.config.ClusterCIDR)
	if err != nil {
		panic(fmt.Errorf("cannot setup tun device: %s", err))
	}

	if certificate != nil {
		if _, err := certificate.GetCertificate(nil); err != nil {
			panic(fmt.Errorf("no TLS certificate: %s", err))
		}
		this.certInfo = NewCertInfo(this.Controller(), certificate)
	}

	var local []net.IPNet
	if this.config.ServiceCIDR != nil {
		local = append(local, *this.config.ServiceCIDR)
	}
	addr := &net.IPNet{
		IP:   this.config.ClusterAddress,
		Mask: this.config.ClusterCIDR.Mask,
	}
	mux := NewMux(this.Controller().GetContext(), this.Controller(), this.certInfo, uint16(this.config.AdvertizedPort), addr, local, tun, this.Links(), this)
	go func() {
		<-this.Controller().GetContext().Done()
		this.Controller().Infof("closing tun device %q", tun)
		tun.Close()
	}()
	this.mux = mux
}

func (this *reconciler) Start() {
	NewServer("broker", this.mux).Start(this.certInfo, "", this.config.Port)
	go func() {
		defer ctxutil.Cancel(this.Controller().GetContext())
		this.Controller().Infof("starting tun server")
		for {
			err := this.mux.HandleTun()
			if err != nil {
				if err == io.EOF {
					this.Controller().Errorf("tun server finished")
				} else {
					this.Controller().Errorf("tun handling aborted: %s", err)
				}
				break
			} else {
				this.mux.tun.Close()
				time.Sleep(100 * time.Millisecond)
				this.Controller().Infof("recreating tun device")
				this.mux.tun, err = NewTun(this.Controller(), this.config.Interface, this.config.ClusterAddress, this.config.ClusterCIDR)
				if err != nil {
					panic(fmt.Errorf("cannot setup tun device: %s", err))
				}
			}
		}
	}()
}

func (this *reconciler) Command(logger logger.LogContext, cmd string) reconcile.Status {
	this.reconcileTun(logger)
	return this.Reconciler.Command(logger, cmd)
}

func (this *reconciler) reconcileTun(logger logger.LogContext) {
	tun := this.mux.tun
	cidr := *this.config.ClusterCIDR
	cidr.IP = this.config.ClusterAddress

	addrs, err := netlink.AddrList(tun.link, netlink.FAMILY_V4)

	for _, a := range addrs {
		if a.IP.Equal(this.config.ClusterAddress) {
			logger.Infof("address still set for %q", tun)
			return
		}
	}

	addr := &netlink.Addr{
		IPNet: &cidr,
	}
	logger.Infof("lost address -> adding address %s to %q", cidr.String(), tun)
	err = netlink.AddrAdd(tun.link, addr)
	if err != nil {
		logger.Errorf("cannot add addr %q to %s: %s", addr, tun, err)
	}

	err = netlink.LinkSetUp(tun.link)
	if err != nil {
		logger.Errorf("cannot bring up %q: %s", tun, err)
	}
}

func (this *reconciler) Notify(l *kubelink.Link, err error) {
	if err != nil {
		this.Controller().Infof("requeue kubelink %q for failure handling: %s", l.Name, err)
	} else {
		this.Controller().Infof("requeue kubelink %q for new connection", l.Name)
	}
	this.Controller().EnqueueKey(resources.NewClusterKey(this.Controller().GetMainCluster().GetId(), v1alpha1.KUBELINK, "", l.Name))
}
