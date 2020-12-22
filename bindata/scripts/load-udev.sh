#!/bin/bash

echo "Trigger udev event"
chroot /host/ /usr/sbin/udevadm trigger --action add --attr-match subsystem=net

