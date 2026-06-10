package com.zkdrive.app.ui

import android.os.Bundle
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.result.contract.ActivityResultContracts
import androidx.activity.viewModels
import androidx.biometric.BiometricManager
import androidx.biometric.BiometricPrompt
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.core.content.ContextCompat
import com.zkdrive.app.R
import androidx.core.splashscreen.SplashScreen.Companion.installSplashScreen
import androidx.fragment.app.FragmentActivity
import androidx.lifecycle.lifecycleScope
import com.zkdrive.app.data.auth.AuthState
import com.zkdrive.app.ui.login.LoginScreen
import com.zkdrive.app.ui.login.LoginViewModel
import com.zkdrive.app.ui.theme.ZkDriveTheme
import dagger.hilt.android.AndroidEntryPoint
import kotlinx.coroutines.flow.launchIn
import kotlinx.coroutines.flow.onEach

/**
 * Single-activity host. Gates the UI on [AuthState]: a splash while restoring
 * the session, the login screen when signed out or locked, and the navigation
 * shell once authenticated. OAuth authorization intents are launched here (the
 * ViewModel cannot start activities), and biometric unlock is driven from the
 * Activity since [BiometricPrompt] needs a [FragmentActivity].
 */
@AndroidEntryPoint
class MainActivity : FragmentActivity() {

    private val appViewModel: AppViewModel by viewModels()
    private val loginViewModel: LoginViewModel by viewModels()

    private val authLauncher = registerForActivityResult(
        ActivityResultContracts.StartActivityForResult(),
    ) { result ->
        result.data?.let { loginViewModel.completeLogin(it) }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        val splash = installSplashScreen()
        enableEdgeToEdge()
        super.onCreate(savedInstanceState)

        // Keep the splash up until the session has been resolved.
        splash.setKeepOnScreenCondition {
            appViewModel.authState.value is AuthState.Loading
        }

        // Launch OAuth authorization intents emitted by the login ViewModel.
        loginViewModel.loginIntents
            .onEach { authLauncher.launch(it) }
            .launchIn(lifecycleScope)

        setContent {
            val theme by appViewModel.theme.collectAsState()
            ZkDriveTheme(themePreference = theme) {
                Surface(
                    modifier = Modifier.fillMaxSize(),
                    color = MaterialTheme.colorScheme.background,
                ) {
                    val authState by appViewModel.authState.collectAsState()
                    when (authState) {
                        is AuthState.Loading -> SplashContent()
                        is AuthState.Authenticated -> ZkApp()
                        AuthState.SignedOut, AuthState.Locked -> LoginScreen(
                            onSignIn = loginViewModel::beginLogin,
                            onUnlock = ::promptBiometricUnlock,
                            viewModel = loginViewModel,
                        )
                    }
                }
            }
        }
    }

    override fun onStop() {
        appViewModel.persistSession()
        super.onStop()
    }

    private fun promptBiometricUnlock() {
        val manager = BiometricManager.from(this)
        val allowed = BiometricManager.Authenticators.BIOMETRIC_STRONG or
            BiometricManager.Authenticators.DEVICE_CREDENTIAL
        if (manager.canAuthenticate(allowed) != BiometricManager.BIOMETRIC_SUCCESS) {
            // No usable biometric/credential — fall back to materialising the
            // session directly (tokens are still encrypted at rest).
            loginViewModel.unlock()
            return
        }

        val prompt = BiometricPrompt(
            this,
            ContextCompat.getMainExecutor(this),
            object : BiometricPrompt.AuthenticationCallback() {
                override fun onAuthenticationSucceeded(result: BiometricPrompt.AuthenticationResult) {
                    loginViewModel.unlock()
                }
            },
        )
        val info = BiometricPrompt.PromptInfo.Builder()
            .setTitle(getString(R.string.login_biometric_title))
            .setSubtitle(getString(R.string.login_biometric_subtitle))
            .setAllowedAuthenticators(allowed)
            .build()
        prompt.authenticate(info)
    }
}

@Composable
private fun SplashContent() {
    Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) {
        CircularProgressIndicator()
    }
}
