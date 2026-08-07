import { useState, useEffect, useCallback, useMemo } from 'react';
import { useParams, useNavigate } from 'react-router-dom';
import { AgentChat, AgentChatProvider, createAgentClient } from '@appx-org/agent-client';
import {
  getProject,
  getServerConfig,
  logout,
  redirectToLogin,
  type Project as ProjectType,
} from '../api/client';
import Terminal from '../components/Terminal';

/**
 * Stable theming overrides for the embedded agent chat. Defined at module scope
 * so the object identity never changes across renders — passing an inline
 * literal would recreate the provider's client/store (and drop the live
 * session) on every re-render.
 */
const CHAT_LABELS = { agentName: 'PI AGENT' };

/** Project is the full-page project view with tabbed Agent/Terminal interface.
 *  The Agent tab uses Pi. The Terminal tab is a local PTY rooted in the
 *  project directory. */
export default function Project() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();

  const [project, setProject] = useState<ProjectType | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [activeTab, setActiveTab] = useState<'agent' | 'terminal'>('agent');
  const [baseDomain, setBaseDomain] = useState('localhost');
  // Preview pane: an iframe of the live dev app (the reverse-proxied subdomain).
  // `previewKey` is bumped to force-reload the iframe on demand.
  const [showPreview, setShowPreview] = useState(true);
  const [previewKey, setPreviewKey] = useState(0);

  // One stable client for the whole project view. The agent-client SDK talks to
  // the same-origin `/api/pi` mirror, which proxies the agent-server `/v1`
  // contract (keeping the bearer token server-side). On 401 we redirect to the
  // login page, matching the rest of the app.
  const agentClient = useMemo(
    () =>
      createAgentClient({
        baseUrl: '/api/pi',
        pathPrefix: '/v1',
        onUnauthorized: redirectToLogin,
      }),
    [],
  );

  const fetchProject = useCallback(async () => {
    if (!id) return;
    try {
      const p = await getProject(id);
      setProject(p);
      setError('');
    } catch (e) {
      if (e instanceof Error && e.message.includes('401')) {
        window.location.href = '/login';
      } else {
        setError(e instanceof Error ? e.message : 'Failed to load project');
      }
    } finally {
      setLoading(false);
    }
  }, [id]);

  useEffect(() => {
    fetchProject();
    getServerConfig()
      .then((cfg) => {
        setBaseDomain(cfg.baseDomain || 'localhost');
      })
      .catch(() => {});
  }, [fetchProject]);

  const projectDir = project?.projectDir ?? '';

  // The reverse proxy routes `<name>.<domain>` to the project's PROD port and
  // `<name>-dev.<domain>` to its DEV port. The preview iframe targets DEV (the
  // live environment the agent iterates on); "Open App" opens PROD.
  const buildUrl = (label: string) => {
    if (!project) return '';
    const proto = window.location.protocol;
    const port = window.location.port;
    const portSuffix = port ? `:${port}` : '';
    return `${proto}//${project.name}${label}.${baseDomain}${portSuffix}/`;
  };
  const subdomainUrl = buildUrl('');
  const devUrl = buildUrl('-dev');

  if (loading) {
    return (
      <div style={styles.container}>
        <div style={styles.centered}><span style={styles.statusLabel}>Loading...</span></div>
      </div>
    );
  }

  if (error || !project) {
    return (
      <div style={styles.container}>
        <div style={styles.centered}>
          <span style={styles.errorText}>{error || 'Project not found'}</span>
          <button style={styles.actionBtn} onClick={() => navigate('/')}>Back to Dashboard</button>
        </div>
      </div>
    );
  }

  return (
    <div style={styles.container}>
      <header style={styles.header}>
        <div style={styles.headerLeft}>
          <button style={styles.backBtn} onClick={() => navigate('/')} aria-label="Back">&#8592;</button>
          <span style={styles.projectName}>{project.name}</span>
          <span style={styles.portLabel}>:{project.assignedPort}</span>
          {project.appRunning && (
            <span style={styles.appBadge}>
              <span style={styles.appDot} />
              APP RUNNING
            </span>
          )}
        </div>
        <div style={styles.headerActions}>
          {project.appRunning && (
            <button
              style={showPreview ? styles.toggleBtnActive : styles.toggleBtn}
              onClick={() => setShowPreview((v) => !v)}
            >
              {showPreview ? 'Hide Preview' : 'Show Preview'}
            </button>
          )}
          {project.appRunning && (
            <a href={subdomainUrl} target="_blank" rel="noopener noreferrer" style={styles.appLink}>
              Open App
            </a>
          )}
          <button style={styles.navBtn}
            onClick={() => logout().then(() => { window.location.href = '/login'; })}>
            Logout
          </button>
        </div>
      </header>

      <div style={styles.main}>
        <div style={styles.workPane}>
          <div style={styles.tabBar}>
            <button style={activeTab === 'agent' ? styles.tabActive : styles.tab} onClick={() => setActiveTab('agent')}>Agent</button>
            <button style={activeTab === 'terminal' ? styles.tabActive : styles.tab} onClick={() => setActiveTab('terminal')}>Terminal</button>
          </div>
          <div style={styles.workBody}>
            {activeTab === 'agent' ? (
              // agent-server addresses projects by their slug id, which equals the
              // appx project *name* (appx names already satisfy the slug grammar).
              <AgentChatProvider client={agentClient} labels={CHAT_LABELS}>
                <AgentChat projectId={project.name} />
              </AgentChatProvider>
            ) : (
              <Terminal cwd={projectDir} />
            )}
          </div>
        </div>

        {project.appRunning && showPreview && (
          <div style={styles.previewPane}>
            <div style={styles.previewBar}>
              <span style={styles.previewUrl}>{devUrl}</span>
              <button
                style={styles.previewReload}
                onClick={() => setPreviewKey((k) => k + 1)}
                aria-label="Reload preview"
                title="Reload preview"
              >
                &#8635;
              </button>
            </div>
            <iframe
              key={previewKey}
              src={devUrl}
              style={styles.previewFrame}
              title={`${project.name} dev preview`}
            />
          </div>
        )}
      </div>
    </div>
  );
}

const styles: Record<string, React.CSSProperties> = {
  container: { height: '100vh', display: 'flex', flexDirection: 'column', overflow: 'hidden' },
  header: { borderBottom: '1px solid var(--border)', padding: '10px 16px', display: 'flex', alignItems: 'center', justifyContent: 'space-between', flexShrink: 0 },
  headerLeft: { display: 'flex', alignItems: 'center', gap: 10 },
  backBtn: { background: 'transparent', border: 'none', color: 'var(--muted)', fontSize: 18, cursor: 'pointer', padding: '0 4px', lineHeight: 1 },
  projectName: { fontSize: 14, fontWeight: 500, color: 'var(--text)' },
  portLabel: { fontFamily: "'JetBrains Mono', monospace", fontSize: 11, color: 'var(--muted)' },
  appBadge: { display: 'flex', alignItems: 'center', gap: 5, fontFamily: "'JetBrains Mono', monospace", fontSize: 10, letterSpacing: '0.07em', color: 'var(--green)' },
  appDot: { width: 6, height: 6, borderRadius: '50%', background: 'var(--green)', flexShrink: 0 },
  headerActions: { display: 'flex', alignItems: 'center', gap: 8 },
  appLink: { padding: '4px 12px', fontSize: 12, color: 'var(--cyan)', textDecoration: 'none', border: '1px solid var(--border)', borderRadius: 4 },
  toggleBtn: { background: 'transparent', border: '1px solid var(--border)', color: 'var(--muted)', padding: '4px 12px', fontSize: 12, cursor: 'pointer', borderRadius: 4 },
  toggleBtnActive: { background: 'var(--surface)', border: '1px solid var(--border)', color: 'var(--text)', padding: '4px 12px', fontSize: 12, cursor: 'pointer', borderRadius: 4 },
  navBtn: { background: 'transparent', border: 'none', color: 'var(--muted)', padding: '5px 10px', fontSize: 12, cursor: 'pointer' },
  tabBar: { display: 'flex', gap: 4, padding: '8px 16px', borderBottom: '1px solid var(--border)', background: 'var(--bg)', flexShrink: 0 },
  tab: { padding: '6px 16px', cursor: 'pointer', border: '1px solid transparent', borderRadius: 4, fontSize: 13, color: 'var(--muted)', background: 'transparent' },
  tabActive: { padding: '6px 16px', cursor: 'pointer', border: '1px solid var(--border)', borderRadius: 4, fontSize: 13, color: 'var(--text)', background: 'var(--surface)' },
  main: { flex: 1, display: 'flex', minHeight: 0, minWidth: 0 },
  previewPane: { flex: 1, display: 'flex', flexDirection: 'column', minWidth: 0, borderLeft: '1px solid var(--border)' },
  previewBar: { display: 'flex', alignItems: 'center', gap: 8, padding: '6px 12px', borderBottom: '1px solid var(--border)', background: 'var(--bg)', flexShrink: 0 },
  previewUrl: { flex: 1, fontFamily: "'JetBrains Mono', monospace", fontSize: 11, color: 'var(--muted)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' },
  previewReload: { background: 'transparent', border: '1px solid var(--border)', color: 'var(--muted)', borderRadius: 4, width: 26, height: 22, fontSize: 14, lineHeight: 1, cursor: 'pointer', flexShrink: 0 },
  previewFrame: { flex: 1, width: '100%', border: 'none', background: 'var(--surface)' },
  workPane: { display: 'flex', flexDirection: 'column', minWidth: 0, minHeight: 0, flex: 1 },
  workBody: { flex: 1, display: 'flex', minHeight: 0 },
  centered: { flex: 1, display: 'flex', flexDirection: 'column', alignItems: 'center', justifyContent: 'center', gap: 16 },
  statusLabel: { fontFamily: "'JetBrains Mono', monospace", fontSize: 13, color: 'var(--muted)', letterSpacing: '0.04em' },
  errorText: { fontFamily: "'JetBrains Mono', monospace", fontSize: 12, color: 'var(--red)', maxWidth: 400, textAlign: 'center' as const, lineHeight: 1.5 },
  actionBtn: { background: 'transparent', border: '1px solid rgba(61,220,132,0.35)', color: 'var(--green)', borderRadius: 4, padding: '6px 20px', fontSize: 12, cursor: 'pointer' },
};
