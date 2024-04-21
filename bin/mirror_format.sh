#!/bin/bash
set -eou pipefail

confirm() {
    (echo -e "\nproceed? y/n" && read -n1 answer && echo && [ $answer = y ] && echo -e '\nproceeding...\n' || (echo -e '\naborting...\n' && exit 1))
}

disks=$(sudo fdisk -l | grep 'Disk /dev/' | cut -d, -f1 | tr -d : | column -t)
echo "$disks"
echo -n 'which device do you want to reformat (and completely erase)? '
read device
echo using device: $device

if ! echo "$disks" | awk '{print $2}' | grep $device &>/dev/null; then
    echo no such device: $device
    exit 1
fi
(
    # gpt table
    echo g
    # main partition
    echo n
    echo 1
    echo
    echo
    echo
    echo w
) | sudo fdisk -w always -W always $device

cryptname=$(echo $device | awk -F'/' '{print $NF}')
echo using cryptname: $cryptname

mount=$(echo ~/mnt.$cryptname)
echo using mount: $mount

if df | awk '{print $NF}' | grep $mount; then
    echo fatal: something already mounted at $mount
    exit 1
fi

confirm

while true; do
    sudo cryptsetup -v luksFormat --type luks2 --key-size=512 --perf-no_read_workqueue --perf-no_write_workqueue ${device}1
    sudo cryptsetup open ${device}1 $cryptname && break
done

sudo mkfs.ext4 /dev/mapper/$cryptname
mkdir -p $mount
sudo mount /dev/mapper/$cryptname $mount
