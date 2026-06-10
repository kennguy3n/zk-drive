package com.zkdrive.app.ui

import androidx.compose.animation.AnimatedVisibility
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Icon
import androidx.compose.material3.NavigationBar
import androidx.compose.material3.NavigationBarItem
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.hilt.navigation.compose.hiltViewModel
import androidx.navigation.NavDestination.Companion.hierarchy
import androidx.navigation.NavGraph.Companion.findStartDestination
import androidx.navigation.NavHostController
import androidx.navigation.NavType
import androidx.navigation.compose.NavHost
import androidx.navigation.compose.composable
import androidx.navigation.compose.currentBackStackEntryAsState
import androidx.navigation.compose.rememberNavController
import androidx.navigation.navArgument
import com.zkdrive.app.ui.browser.BrowserScreen
import com.zkdrive.app.ui.navigation.ResourceTypes
import com.zkdrive.app.ui.navigation.Routes
import com.zkdrive.app.ui.navigation.TopLevelDestination
import com.zkdrive.app.ui.preview.PreviewScreen
import com.zkdrive.app.ui.search.SearchScreen
import com.zkdrive.app.ui.settings.SettingsScreen
import com.zkdrive.app.ui.share.ShareScreen

/**
 * The authenticated app shell: a bottom-nav Scaffold hosting the Files /
 * Search / Settings destinations, plus full-screen Preview and Share routes
 * layered on top (no bottom bar). Each screen gets its own Hilt ViewModel.
 */
@Composable
fun ZkApp(
    navController: NavHostController = rememberNavController(),
) {
    val backStackEntry by navController.currentBackStackEntryAsState()
    // Registered routes carry their argument template (e.g.
    // "browser?folderId={folderId}&folderName={folderName}"); strip the query
    // template so we match against the bare destination ids in TOP_LEVEL_ROUTES.
    val currentRoute = backStackEntry?.destination?.route?.substringBefore('?')
    val showBottomBar = currentRoute in TOP_LEVEL_ROUTES

    Scaffold(
        bottomBar = {
            AnimatedVisibility(visible = showBottomBar) {
                NavigationBar {
                    val destination = backStackEntry?.destination
                    TopLevelDestination.entries.forEach { dest ->
                        val selected = destination?.hierarchy?.any {
                            it.route?.substringBefore('?') == dest.route
                        } == true
                        NavigationBarItem(
                            selected = selected,
                            onClick = {
                                navController.navigate(dest.route) {
                                    popUpTo(navController.graph.findStartDestination().id) {
                                        saveState = true
                                    }
                                    launchSingleTop = true
                                    restoreState = true
                                }
                            },
                            icon = { Icon(dest.icon, contentDescription = dest.label) },
                            label = { Text(dest.label) },
                        )
                    }
                }
            }
        },
    ) { padding ->
        NavHost(
            navController = navController,
            startDestination = Routes.BROWSER,
            modifier = Modifier.padding(padding),
        ) {
            composable(
                route = Routes.BROWSER_PATTERN,
                arguments = listOf(
                    navArgument(Routes.ARG_FOLDER_ID) { type = NavType.StringType; nullable = true; defaultValue = null },
                    navArgument(Routes.ARG_FOLDER_NAME) { type = NavType.StringType; nullable = true; defaultValue = null },
                ),
            ) {
                BrowserScreen(
                    onOpenFile = { file ->
                        navController.navigate(Routes.preview(file.id, file.name, file.mimeType))
                    },
                    onShare = { type, id, name ->
                        navController.navigate(Routes.share(type, id, name))
                    },
                    viewModel = hiltViewModel(),
                )
            }

            composable(Routes.SEARCH) {
                SearchScreen(
                    onOpenResult = { hit ->
                        if (hit.isFolder) {
                            // Deep-link straight into the tapped folder so search
                            // actually navigates into it (not just to the root).
                            navController.navigate(Routes.browser(hit.id, hit.name)) {
                                popUpTo(navController.graph.findStartDestination().id) { saveState = true }
                                launchSingleTop = true
                            }
                        } else {
                            navController.navigate(Routes.preview(hit.id, hit.name, ""))
                        }
                    },
                    viewModel = hiltViewModel(),
                )
            }

            composable(Routes.SETTINGS) {
                SettingsScreen(viewModel = hiltViewModel())
            }

            composable(
                route = Routes.PREVIEW_PATTERN,
                arguments = listOf(
                    navArgument(Routes.ARG_FILE_ID) { type = NavType.StringType },
                    navArgument(Routes.ARG_FILE_NAME) { type = NavType.StringType; defaultValue = "" },
                    navArgument(Routes.ARG_FILE_MIME) { type = NavType.StringType; defaultValue = "" },
                ),
            ) {
                PreviewScreen(onBack = navController::popBackStack, viewModel = hiltViewModel())
            }

            composable(
                route = Routes.SHARE_PATTERN,
                arguments = listOf(
                    navArgument(Routes.ARG_RESOURCE_TYPE) {
                        type = NavType.StringType; defaultValue = ResourceTypes.FILE
                    },
                    navArgument(Routes.ARG_RESOURCE_ID) { type = NavType.StringType },
                    navArgument(Routes.ARG_RESOURCE_NAME) { type = NavType.StringType; defaultValue = "" },
                ),
            ) {
                ShareScreen(onBack = navController::popBackStack, viewModel = hiltViewModel())
            }
        }
    }
}

private val TOP_LEVEL_ROUTES = TopLevelDestination.entries.map { it.route }.toSet()
