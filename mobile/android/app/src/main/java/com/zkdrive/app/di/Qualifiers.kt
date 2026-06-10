package com.zkdrive.app.di

import javax.inject.Qualifier

/** Marks the IO-bound coroutine dispatcher (network + disk). */
@Qualifier
@Retention(AnnotationRetention.BINARY)
annotation class IoDispatcher

/** Marks the application-lifetime coroutine scope. */
@Qualifier
@Retention(AnnotationRetention.BINARY)
annotation class ApplicationScope
