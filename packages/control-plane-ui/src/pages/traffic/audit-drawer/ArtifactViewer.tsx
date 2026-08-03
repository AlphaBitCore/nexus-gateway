import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { api, ApiError } from '@/api/client';
import styles from './ArtifactViewer.module.css';

/**
 * ArtifactViewer renders the captured multimodal artifact for an image or TTS
 * traffic event inline: <img> for image generations, <audio controls> for
 * speech. It fetches the bytes through the authenticated API client (an <img
 * src> cannot attach the bearer token) into an object URL, and revokes it on
 * unmount. A url-mode image (the gateway never fetched it → 409) shows a notice
 * pointing to the response body; a row with no servable artifact renders
 * nothing.
 */
export function ArtifactViewer({ eventId, endpointType }: { eventId: string; endpointType?: string | null }) {
  const { t } = useTranslation();
  const [objectUrl, setObjectUrl] = useState<string | null>(null);
  const [mime, setMime] = useState<string>('');
  const [state, setState] = useState<'loading' | 'ready' | 'url' | 'none' | 'error'>('loading');

  const previewable = endpointType === 'image_generation' || endpointType === 'tts';

  useEffect(() => {
    if (!previewable) {
      setState('none');
      return;
    }
    let revoked = false;
    let createdUrl: string | null = null;
    setState('loading');
    api
      .getBlob(`/traffic/${eventId}/artifact`)
      .then(({ blob }) => {
        if (revoked) return;
        createdUrl = URL.createObjectURL(blob);
        setObjectUrl(createdUrl);
        setMime(blob.type);
        setState('ready');
      })
      .catch((err: unknown) => {
        if (revoked) return;
        if (err instanceof ApiError && err.status === 409) {
          setState('url'); // url-mode image: the reference is in the response body
        } else if (err instanceof ApiError && err.status === 404) {
          setState('none'); // no inline artifact for this event
        } else {
          setState('error');
        }
      });
    return () => {
      revoked = true;
      if (createdUrl) URL.revokeObjectURL(createdUrl);
    };
  }, [eventId, previewable]);

  if (!previewable || state === 'none') return null;

  let inner;
  if (state === 'loading') {
    inner = <div className={styles.notice}>{t('pages:traffic.detail.artifact.loading', 'Loading preview…')}</div>;
  } else if (state === 'url') {
    inner = <div className={styles.notice}>{t('pages:traffic.detail.artifact.urlMode', 'The provider returned this image as a URL (not stored) — see the response body.')}</div>;
  } else if (state === 'error') {
    inner = <div className={styles.notice}>{t('pages:traffic.detail.artifact.error', 'Preview unavailable.')}</div>;
  } else if (mime.startsWith('image/') && objectUrl) {
    inner = <img className={styles.image} src={objectUrl} alt={t('pages:traffic.detail.artifact.imageAlt', 'Generated image')} />;
  } else if (mime.startsWith('audio/') && objectUrl) {
    inner = <audio className={styles.audio} controls src={objectUrl} />;
  } else {
    inner = <div className={styles.notice}>{t('pages:traffic.detail.artifact.error', 'Preview unavailable.')}</div>;
  }

  return (
    <div className={styles.wrap}>
      <div className={styles.label}>{t('pages:traffic.detail.artifact.title', 'Artifact')}</div>
      {inner}
    </div>
  );
}
