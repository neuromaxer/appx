import { useMemo, type CSSProperties } from 'react';
import { useNavigate } from 'react-router-dom';
import { AgentChatProvider, AgentSettings, createAgentClient } from '@appx-org/agent-client';
import { logout, redirectToLogin } from '../api/client';

/**
 * Settings page chrome. The provider-credential management UI itself lives in
 * agent-client (`AgentSettings`), so it stays reusable across any app on
 * agent-client + agent-server. appx only supplies the surrounding header,
 * the same-origin `/api/pi` transport, and the egress shortcut.
 */
export default function Settings() {
  const navigate = useNavigate();

  // One stable client for the settings view, talking to the same-origin
  // `/api/pi` mirror (agent-server `/v1` contract; bearer token stays
  // server-side). On 401 we redirect to login, matching the rest of the app.
  const agentClient = useMemo(
    () =>
      createAgentClient({
        baseUrl: '/api/pi',
        pathPrefix: '/v1',
        onUnauthorized: redirectToLogin,
      }),
    [],
  );

  return (
    <div style={styles.container}>
      <header style={styles.header}>
        <span style={styles.wordmark}>APPX</span>
        <div style={styles.headerActions}>
          <button
            data-btn="text-nav"
            style={{ ...styles.navBtn, color: 'var(--muted)' }}
            onClick={() => logout().then(() => { window.location.href = '/login'; })}
          >
            Logout
          </button>
        </div>
      </header>

      <main style={styles.main}>
        <div style={styles.pageHeader}>
          <button style={styles.backBtn} onClick={() => navigate('/')} aria-label="Back to dashboard">&#8592;</button>
          <span style={styles.pageTitle}>SETTINGS</span>
        </div>

        <AgentChatProvider client={agentClient}>
          <AgentSettings description="Pi auth for the agent service user." />
        </AgentChatProvider>

        <div style={{ ...styles.card, marginTop: 16, cursor: 'pointer' }} onClick={() => navigate('/egress')}>
          <h3 style={styles.cardTitle}>Egress Control</h3>
          <p style={{ ...styles.description, margin: 0 }}>
            View outbound connection log and manage the allowlist for agent network access.
          </p>
        </div>
      </main>
    </div>
  );
}

const styles: Record<string, CSSProperties> = {
  container: {
    minHeight: '100vh',
  },
  header: {
    borderBottom: '1px solid var(--border)',
    padding: '14px 24px',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
  },
  wordmark: {
    fontFamily: "'DM Sans', sans-serif",
    fontSize: 14,
    fontWeight: 500,
    letterSpacing: '0.35em',
    color: 'var(--text)',
  },
  headerActions: {
    display: 'flex',
    alignItems: 'center',
    gap: 4,
  },
  navBtn: {
    background: 'transparent',
    border: 'none',
    color: 'var(--muted)',
    padding: '5px 10px',
    fontSize: 13,
    cursor: 'pointer',
  },
  main: {
    padding: '28px 24px',
    maxWidth: 840,
    margin: '0 auto',
  },
  pageHeader: {
    display: 'flex',
    alignItems: 'center',
    gap: 10,
    marginBottom: 20,
  },
  backBtn: {
    background: 'transparent',
    border: 'none',
    color: 'var(--muted)',
    fontSize: 20,
    cursor: 'pointer',
    padding: '0 4px',
    lineHeight: 1,
  },
  pageTitle: {
    fontFamily: "'JetBrains Mono', monospace",
    fontSize: 11,
    letterSpacing: '0.12em',
    color: 'var(--muted)',
  },
  card: {
    background: 'var(--surface)',
    border: '1px solid var(--border)',
    borderRadius: 4,
    padding: '20px 22px',
  },
  cardTitle: {
    margin: '0 0 8px',
    fontSize: 14,
    fontWeight: 500,
    color: 'var(--text)',
  },
  description: {
    color: 'var(--muted)',
    fontSize: 13,
    lineHeight: 1.6,
    margin: '0 0 18px',
  },
};
