ARG AGENT_IMAGE=pillar-csi/agent:docker-e2e
FROM ${AGENT_IMAGE} AS agent

FROM alpine:3.24
RUN apk add --no-cache e2fsprogs kmod lvm2 util-linux \
    && sed -i 's/obtain_device_list_from_udev = 1/obtain_device_list_from_udev = 0/' /etc/lvm/lvm.conf \
    && sed -i 's/udev_sync = 1/udev_sync = 0/' /etc/lvm/lvm.conf \
    && sed -i 's/udev_rules = 1/udev_rules = 0/' /etc/lvm/lvm.conf
COPY --from=agent /usr/bin/pillar-agent /usr/local/bin/pillar-agent
COPY test/docker-e2e/external-agent-entrypoint.sh /usr/local/bin/external-agent-entrypoint
RUN chmod 0555 /usr/local/bin/pillar-agent /usr/local/bin/external-agent-entrypoint
ENTRYPOINT ["/usr/local/bin/external-agent-entrypoint"]
