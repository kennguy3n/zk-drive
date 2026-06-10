package com.zkdrive.app.push

/**
 * Stable notification-channel IDs. These mirror the string resources used to
 * create the channels in [com.zkdrive.app.ZkDriveApplication] and are
 * referenced when posting notifications.
 */
object NotificationChannels {
    const val PUSH = "zk_push"
    const val TRANSFERS = "zk_transfers"
    const val SYNC = "zk_sync"
}
