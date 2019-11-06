#
# This is the OpenShift ovn overlay network image.
# it provides an overlay network using ovs/ovn/ovn-kube
#
# The standard name for this image is ovn-kube

# Notes:
# This is for a build where the ovn-kubernetes utilities
# are built in this Dockerfile and included in the image (instead of the rpm)
#

FROM openshift/origin-release:golang-1.12 AS builder

WORKDIR /go-controller
COPY go-controller/ .

# build the binaries
RUN CGO_ENABLED=0 make

FROM openshift/origin-cli AS cli

FROM openshift/origin-base

USER root

ENV PYTHONDONTWRITEBYTECODE yes

# install needed rpms - openvswitch must be 2.10.4 or higher
# install selinux-policy first to avoid a race
RUN yum install -y  \
	selinux-policy && \
	yum clean all

RUN INSTALL_PKGS=" \
	PyYAML bind-utils openssl firewalld-filesystem \
	libpcap  hostname iproute strace socat \
	containernetworking-plugins \
	" && \
	yum install -y --setopt=tsflags=nodocs --setopt=skip_missing_names_on_install=False $INSTALL_PKGS && \
	rpm -i \
               http://www.russellbryant.net/openvswitch2.11/2.11.0/26.el7fdp/x86_64/openvswitch2.11-2.11.0-26.el7fdp.x86_64.rpm \
               http://www.russellbryant.net/openvswitch2.11/2.11.0/26.el7fdp/x86_64/openvswitch2.11-devel-2.11.0-26.el7fdp.x86_64.rpm \
               http://www.russellbryant.net/openvswitch2.11/2.11.0/26.el7fdp/x86_64/python-openvswitch2.11-2.11.0-26.el7fdp.x86_64.rpm \
               https://markmc.fedorapeople.org/ovn-2.11.1-12.el7fdn/ovn2.11-2.11.1-12.el7fdn.x86_64.rpm \
               https://markmc.fedorapeople.org/ovn-2.11.1-12.el7fdn/ovn2.11-debuginfo-2.11.1-12.el7fdn.x86_64.rpm \
               https://markmc.fedorapeople.org/ovn-2.11.1-12.el7fdn/ovn2.11-central-2.11.1-12.el7fdn.x86_64.rpm \
               https://markmc.fedorapeople.org/ovn-2.11.1-12.el7fdn/ovn2.11-host-2.11.1-12.el7fdn.x86_64.rpm \
               https://markmc.fedorapeople.org/ovn-2.11.1-12.el7fdn/ovn2.11-vtep-2.11.1-12.el7fdn.x86_64.rpm && \
	yum clean all && rm -rf /var/cache/*

RUN mkdir -p /var/run/openvswitch && \
    mkdir -p /etc/cni/net.d && \
    mkdir -p /opt/cni/bin && \
    mkdir -p /usr/libexec/cni/

COPY --from=builder /go-controller/_output/go/bin/ovnkube /usr/bin/
COPY --from=builder /go-controller/_output/go/bin/ovn-kube-util /usr/bin/
COPY --from=builder /go-controller/_output/go/bin/ovn-k8s-cni-overlay /usr/libexec/cni/ovn-k8s-cni-overlay

COPY --from=cli /usr/bin/oc /usr/bin
RUN ln -s /usr/bin/oc /usr/bin/kubectl

# copy git commit number into image
COPY .git/HEAD /root/.git/HEAD
COPY .git/refs/heads/ /root/.git/refs/heads/

# ovnkube.sh is the entry point. This script examines environment
# variables to direct operation and configure ovn
COPY dist/images/ovnkube.sh /root/
COPY dist/images/ovn-debug.sh /root/

# iptables wrappers
COPY ./dist/images/iptables-scripts/iptables /usr/sbin/
COPY ./dist/images/iptables-scripts/iptables-save /usr/sbin/
COPY ./dist/images/iptables-scripts/iptables-restore /usr/sbin/
COPY ./dist/images/iptables-scripts/ip6tables /usr/sbin/
COPY ./dist/images/iptables-scripts/ip6tables-save /usr/sbin/
COPY ./dist/images/iptables-scripts/ip6tables-restore /usr/sbin/
COPY ./dist/images/iptables-scripts/iptables /usr/sbin/

LABEL io.k8s.display-name="ovn kubernetes" \
      io.k8s.description="This is a component of OpenShift Container Platform that provides an overlay network using ovn." \
      summary="This is a component of OpenShift Container Platform that provides an overlay network using ovn." \
      io.openshift.tags="openshift" \
      maintainer="Phil Cameron <pcameron@redhat.com>"

WORKDIR /root
ENTRYPOINT /root/ovnkube.sh

