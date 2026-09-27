import logger from './lib/logger';

const PORT = process.env.PORT || 3001;

/**
 * CLI command handlers
 */
async function handleLogin(args: string[]): Promise<void> {
  const { authService } = await import('./services/auth');
  // Parse arguments
  let serverUrl = `http://localhost:${PORT}`;
  let contextName: string | undefined;
  
  for (let i = 0; i < args.length; i++) {
    if (args[i] === '--server' || args[i] === '-s') {
      serverUrl = args[i + 1] || serverUrl;
      i++;
    } else if (args[i] === '--context' || args[i] === '-c') {
      contextName = args[i + 1];
      i++;
    }
  }

  console.log('🔐 AI Runway Login\n');
  console.log('Extracting credentials from kubeconfig...');

  const credentials = await authService.extractTokenFromKubeconfig(contextName);
  
  if (!credentials) {
    console.error('\n❌ Failed to extract token from kubeconfig.');
    console.error('\nMake sure your kubeconfig has valid OIDC or token-based authentication.');
    console.error('Try running: kubectl get pods  (to refresh your credentials)');
    process.exit(1);
  }

  // Save credentials locally
  authService.saveCredentials(credentials);

  // Generate login URL
  const loginUrl = authService.generateLoginUrl(serverUrl, credentials.token);

  console.log(`\n✅ Logged in as: ${credentials.username}`);
  if (credentials.expiresAt) {
    console.log(`   Token expires: ${new Date(credentials.expiresAt).toLocaleString()}`);
  }

  console.log('\n📋 Open this URL in your browser to authenticate:\n');
  console.log(`   ${loginUrl}\n`);

  // Try to open browser automatically
  try {
    const openCommand = process.platform === 'darwin' ? 'open' : 
                        process.platform === 'win32' ? 'start' : 'xdg-open';
    
    const proc = Bun.spawn([openCommand, loginUrl], {
      stdout: 'ignore',
      stderr: 'ignore',
    });
    await proc.exited;
    console.log('   (Browser opened automatically)');
  } catch {
    console.log('   (Copy and paste the URL above into your browser)');
  }
}

async function handleLogout(): Promise<void> {
  const { authService } = await import('./services/auth');
  authService.clearCredentials();
  console.log('✅ Logged out. Credentials cleared.');
}

async function startServer(): Promise<void> {
  const { default: app } = await import('./hono-app');
  const { authService } = await import('./services/auth');
  const server = Bun.serve({
    port: Number(PORT),
    fetch: app.fetch,
    // Increase idle timeout for long-running operations like Helm installs
    // Max value is 255 seconds (~4 minutes)
    idleTimeout: 255,
  });

  const authEnabled = authService.isAuthEnabled();
  
  logger.info({ port: server.port, authEnabled }, `🚀 AI Runway backend running on http://localhost:${server.port}`);
  
  if (authEnabled) {
    console.log('\n🔐 Authentication is ENABLED');
    console.log(`   Run: airunway login --server http://localhost:${server.port}\n`);
  }
}

// Main CLI entry point
async function main(): Promise<void> {
  const args = process.argv.slice(2);
  const command = args[0] || 'serve';

  switch (command) {
    case 'serve':
      await startServer();
      break;
    case 'login':
      await handleLogin(args.slice(1));
      break;
    case 'logout':
      await handleLogout();
      break;
    case 'version':
    case '--version':
    case '-v': {
      const { BUILD_INFO } = await import('./build-info');
      console.log(`AI Runway dashboard ${BUILD_INFO.version} (${BUILD_INFO.gitCommit})`);
      break;
    }
    case 'help':
    case '--help':
    case '-h':
      console.log('Usage: airunway-web [serve|login|logout|version]\n\nModel and agent commands are provided by the standalone Go airunway CLI.');
      break;
    default:
      console.error('Unknown dashboard command. Run airunway-web --help.');
      process.exitCode = 2;
  }
}

main().catch((error) => {
  logger.error({ error }, 'Fatal error');
  process.exit(1);
});
